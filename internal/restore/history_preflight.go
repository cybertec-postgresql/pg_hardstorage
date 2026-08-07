//go:build !mutation_history_preflight_absent

package restore

// history_preflight.go — a `--to-latest` restore must be able to REACH
// the latest timeline, and PostgreSQL will not say so when it can't.
//
// findNewestTimeLine probes `<N>.history` ascending and stops at the
// FIRST miss: a single unreachable history file silently caps recovery
// at the older timeline. The archive can hold every WAL segment of
// TLI 5 and recovery still promotes on TLI 4 — no error, no warning,
// "success" — because 5.history was never archived (a promotion race,
// a lost spool, an operator's cleanup) or was lost later. The same
// blindness applies to a pinned `--timeline N`: PG needs N.history for
// the ancestry walk, and without it recovery fails into fallback
// behaviour the operator never asked for.
//
// This preflight closes the gap the same way the WAL-gap checks do:
// enumerate the timelines the archive actually holds segments for,
// and refuse — typed, before any data movement — when a history file
// needed to reach the requested timeline is in neither of the two
// places `wal fetch` serves them from (the archive_command aux path
// and the streaming follower's timeline store). --skip-gap-check
// remains the eyes-open override, consistent with every other
// recovery preflight.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/timeline"
)

func preflightTimelineHistory(ctx context.Context, sp storage.StoragePlugin, deployment string, seedTLI uint32, recovery *Recovery, emit func(*output.Event)) error {
	if recovery == nil || !recovery.Enable || recovery.SkipGapCheck {
		return nil
	}

	// Which timelines must be reachable?
	var required []uint32
	switch {
	case recovery.Timeline == "" || recovery.Timeline == "latest":
		maxTLI, err := highestArchivedTimeline(ctx, sp, deployment)
		if err != nil {
			// Storage trouble degrades to a warning, same posture as
			// the gap preflight: a transient List failure must not
			// tank a legitimate restore.
			if emit != nil {
				emit(output.NewEvent(output.SeverityWarning, "restore", "timeline_scan_failed").
					WithSubject(output.Subject{Deployment: deployment}).
					WithBody(map[string]any{"error": err.Error()}))
			}
			return nil
		}
		// Every timeline between the seed and the top must be
		// PROBE-reachable: PG walks ascending and stops at the first
		// miss, so a hole below the top caps recovery below it even
		// when the top's own history is present.
		for tli := seedTLI + 1; tli <= maxTLI; tli++ {
			required = append(required, tli)
		}
	default:
		t, perr := strconv.ParseUint(recovery.Timeline, 10, 32)
		if perr != nil {
			return nil // recovery.Validate owns the complaint
		}
		if uint32(t) > seedTLI {
			// A pinned target timeline needs its own history file —
			// it carries the full ancestry chain.
			required = append(required, uint32(t))
		}
	}

	var missing []uint32
	for _, tli := range required {
		ok, err := timelineHistoryReachable(ctx, sp, deployment, tli)
		if err != nil {
			if emit != nil {
				emit(output.NewEvent(output.SeverityWarning, "restore", "timeline_scan_failed").
					WithSubject(output.Subject{Deployment: deployment, Timeline: tli}).
					WithBody(map[string]any{"error": err.Error()}))
			}
			continue // probe trouble ≠ proven absence; degrade
		}
		if !ok {
			missing = append(missing, tli)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	names := make([]string, 0, len(missing))
	for _, tli := range missing {
		names = append(names, fmt.Sprintf("%08X.history", tli))
	}
	return output.NewError("restore.timeline_history_unreachable",
		fmt.Sprintf("restore: recovery to timeline %q cannot reach the archive's newest timeline: %s missing from both the archive path and the timeline store. PostgreSQL probes history files ascending and STOPS AT THE FIRST MISS, so it would silently end recovery on an older timeline and promote — reporting success while every segment archived on the newer timeline(s) is ignored",
			timelineWord(recovery.Timeline), strings.Join(names, ", "))).
		WithSuggestion(&output.Suggestion{
			Human:   "re-archive the missing history file(s) with `pg_hardstorage wal push " + deployment + " <path/to/NNNNNNNN.history>` (any cluster member's pg_wal usually still has them), or pin --timeline to the highest timeline whose history chain is complete if the newer timelines are genuinely unwanted, or pass --skip-gap-check to accept recovery that silently stops on an older timeline.",
			Command: "pg_hardstorage wal push " + deployment + " <NNNNNNNN.history>",
		})
}

func timelineWord(t string) string {
	if t == "" {
		return "latest"
	}
	return t
}

// timelineHistoryReachable reports whether <tli>.history can be served
// by `wal fetch` — from the archive_command aux path or the streaming
// follower's timeline store, the same two locations fetchAuxBody
// consults at recovery time.
func timelineHistoryReachable(ctx context.Context, sp storage.StoragePlugin, deployment string, tli uint32) (bool, error) {
	name := fmt.Sprintf("%08X.history", tli)
	if _, err := sp.Stat(ctx, walsink.AuxiliaryFilePath(deployment, name, walsink.AuxiliaryHistory)); err == nil {
		return true, nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return false, err
	}
	if _, err := timeline.New(sp).Get(ctx, deployment, tli); err == nil {
		return true, nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return false, err
	}
	return false, nil
}

// highestArchivedTimeline scans wal/<deployment>/ for segment-manifest
// keys and returns the highest timeline that has at least one. Zero
// means no archived segments at all.
func highestArchivedTimeline(ctx context.Context, sp storage.StoragePlugin, deployment string) (uint32, error) {
	prefix := "wal/" + deployment + "/"
	var maxTLI uint32
	for info, lerr := range sp.List(ctx, prefix) {
		if lerr != nil {
			return 0, lerr
		}
		if cerr := ctx.Err(); cerr != nil {
			return 0, cerr
		}
		rest := strings.TrimPrefix(info.Key, prefix)
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || len(parts[0]) != 8 {
			continue // gaps/, history/, timelines/, aux oddities
		}
		if !strings.HasSuffix(parts[1], ".json") || strings.Contains(parts[1], ".json.tmp.") ||
			strings.Contains(parts[1], "/") {
			continue
		}
		base := strings.TrimSuffix(parts[1], ".json")
		if len(base) != 24 {
			continue // .backup/.partial aux land beside segments
		}
		tli64, perr := strconv.ParseUint(parts[0], 16, 32)
		if perr != nil {
			continue
		}
		if uint32(tli64) > maxTLI {
			maxTLI = uint32(tli64)
		}
	}
	return maxTLI, nil
}
