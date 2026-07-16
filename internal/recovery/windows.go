// windows.go — PITRWindow: per-backup recoverable time/LSN range (stop_lsn → highest archived WAL).
package recovery

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/inventory"
)

// WindowsSchema is the on-disk version tag for WindowsReport bodies.
const WindowsSchema = "pg_hardstorage.recovery.windows.v1"

// PITRWindow describes one backup's recoverable range.
//
// "Window" here means: starting from this backup, what time / LSN
// range can a PITR restore land in? The lower bound is StopLSN
// (the backup's commit point); the upper bound is the highest
// archived WAL LSN on the same timeline (since PITR replays WAL on
// top of the base).
//
// Gaps within the window are surfaced explicitly — even if the
// upper bound is fresh, a gap in the middle makes recovery within
// that range impossible.
type PITRWindow struct {
	BackupID       string    `json:"backup_id"`
	BackupType     string    `json:"backup_type"`
	StoppedAt      time.Time `json:"stopped_at"`
	StartLSN       string    `json:"start_lsn"`
	StopLSN        string    `json:"stop_lsn"`
	Timeline       uint32    `json:"timeline"`
	HasReplicaCopy bool      `json:"has_replica_copy"`

	// EarliestRestoreLSN is the lowest LSN this backup can serve
	// (always = StopLSN). Anything before is "use the prior
	// backup".
	EarliestRestoreLSN string `json:"earliest_restore_lsn"`

	// LatestRestoreLSN is the highest LSN reachable by replaying
	// archived WAL on top of this backup. Empty when no WAL is
	// archived for the backup's timeline (a fresh deployment with
	// just the base; PITR isn't possible past StopLSN).
	LatestRestoreLSN string `json:"latest_restore_lsn,omitempty"`

	// HasArchivedWAL reports whether ANY WAL is archived for this
	// timeline. False = base-only restore is possible; PITR is not.
	HasArchivedWAL bool `json:"has_archived_wal"`

	// Gaps lists every persisted gap that overlaps the window.
	// Empty when no gaps are recorded.
	Gaps []WindowGap `json:"gaps,omitempty"`

	// WALGapsFromManifest lists gaps embedded in the manifest at
	// commit time (the wal_gaps field). These persist regardless
	// of whether the live gapstate was GC'd.
	WALGapsFromManifest []WindowGap `json:"wal_gaps_from_manifest,omitempty"`
}

// WindowGap is one PITR-breaking WAL gap within a window.
type WindowGap struct {
	StartLSN   string    `json:"start_lsn"`
	EndLSN     string    `json:"end_lsn"`
	Bytes      uint64    `json:"bytes"`
	DetectedAt time.Time `json:"detected_at,omitempty"`
	SlotName   string    `json:"slot_name,omitempty"`
	SlotRole   string    `json:"slot_role,omitempty"`
	Source     string    `json:"source"` // "manifest" | "gapstate"
}

// WindowsCoverage aggregates fleet-level recoverability metrics.
type WindowsCoverage struct {
	WindowCount             int       `json:"window_count"`
	EarliestRecoverableTime time.Time `json:"earliest_recoverable_time,omitempty"`
	LatestRecoverableTime   time.Time `json:"latest_recoverable_time,omitempty"`
	TotalGapBytes           uint64    `json:"total_gap_bytes,omitempty"`
	WindowsWithGaps         int       `json:"windows_with_gaps,omitempty"`
}

// WindowsReport is the structured PITR-windows enumeration.
type WindowsReport struct {
	Schema      string    `json:"schema"`
	GeneratedAt time.Time `json:"generated_at"`
	StoppedAt   time.Time `json:"stopped_at"`
	DurationMS  int64     `json:"duration_ms"`

	URL        string `json:"url"`
	Deployment string `json:"deployment"`

	Windows  []PITRWindow    `json:"windows"`
	Coverage WindowsCoverage `json:"coverage"`
}

// WindowsOptions configures one Windows() run.
type WindowsOptions struct {
	// Verifier is required.
	Verifier *backup.Verifier

	// Now overrides time.Now() for deterministic test output.
	Now time.Time

	// IncludeOlderThan controls how far back we walk (zero = all).
	// Useful for "show me recoverable windows in the last 90 days".
	IncludeOlderThan time.Duration
}

// Windows enumerates PITR windows for a deployment.
func Windows(ctx context.Context, sp storage.StoragePlugin, deployment string, opts WindowsOptions) (*WindowsReport, error) {
	if sp == nil {
		return nil, errors.New("recovery: nil StoragePlugin")
	}
	if opts.Verifier == nil {
		return nil, errors.New("recovery: Verifier is required")
	}
	if deployment == "" {
		return nil, errors.New("recovery: deployment is required")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	started := time.Now().UTC()
	r := &WindowsReport{
		Schema:      WindowsSchema,
		GeneratedAt: now,
		Deployment:  deployment,
	}
	finish := func() {
		r.StoppedAt = time.Now().UTC()
		r.DurationMS = r.StoppedAt.Sub(started).Milliseconds()
	}

	store := backup.NewManifestStore(sp)

	// Pre-load persisted gaps so we don't re-list per backup.
	persistedGap, gapPresent, gapErr := gapstate.New(sp).LatestAny(ctx, deployment)
	_ = gapErr // best-effort

	// Per-timeline highest archived LSN cache so the loop below
	// doesn't re-walk the segments listing per backup.
	highestByTimeline := map[uint32]string{}

	cutoff := time.Time{}
	if opts.IncludeOlderThan > 0 {
		cutoff = now.Add(-opts.IncludeOlderThan)
	}

	for m, lerr := range store.List(ctx, deployment, opts.Verifier) {
		if err := ctx.Err(); err != nil {
			finish()
			return r, err
		}
		if lerr != nil {
			continue
		}
		if !cutoff.IsZero() && m.StoppedAt.Before(cutoff) {
			continue
		}
		w := PITRWindow{
			BackupID:           m.BackupID,
			BackupType:         string(m.Type),
			StoppedAt:          m.StoppedAt,
			StartLSN:           m.StartLSN,
			StopLSN:            m.StopLSN,
			Timeline:           m.Timeline,
			EarliestRestoreLSN: m.StopLSN,
			HasReplicaCopy:     replicaPresent(sp, m.BackupID),
		}
		// Highest archived LSN on the manifest's timeline (cached
		// across iterations).
		hi, cached := highestByTimeline[m.Timeline]
		if !cached {
			h, found, err := walHighestForTimeline(ctx, sp, deployment, m.Timeline)
			if err == nil && found {
				hi = h
				highestByTimeline[m.Timeline] = hi
			} else {
				highestByTimeline[m.Timeline] = ""
			}
		}
		if hi != "" {
			w.LatestRestoreLSN = hi
			w.HasArchivedWAL = true
			// Contiguity cap: hi is only the HIGHEST archived segment —
			// it does not mean every segment between the backup and hi
			// is present. If there's an archive hole in that range,
			// replay HALTS at the hole, so the real latest-reachable LSN
			// is the hole's start, not hi. Without this the window
			// advertised a PITR range straight across a gap that
			// `wal list --gaps-only` plainly detects.
			fromLSN, ferr := pglogrepl.ParseLSN(m.StopLSN)
			toLSN, terr := pglogrepl.ParseLSN(hi)
			if ferr == nil && terr == nil && toLSN > fromLSN {
				if hole, found, herr := inventory.FirstWALHoleInRange(ctx, sp, deployment, m.Timeline, fromLSN, toLSN); herr == nil && found {
					w.LatestRestoreLSN = hole.String()
					w.Gaps = append(w.Gaps, WindowGap{
						StartLSN: hole.String(),
						EndLSN:   hi,
						Source:   "archive_scan",
					})
				}
			}
		}
		// Manifest-embedded gaps survive gapstate GC.
		for _, g := range m.WALGaps {
			w.WALGapsFromManifest = append(w.WALGapsFromManifest, WindowGap{
				StartLSN:   g.GapStartLSN,
				EndLSN:     g.GapEndLSN,
				Bytes:      g.GapBytes,
				DetectedAt: g.DetectedAt,
				SlotName:   g.SlotName,
				SlotRole:   g.SlotRole,
				Source:     "manifest",
			})
			r.Coverage.TotalGapBytes += g.GapBytes
		}
		// Persisted gap on this timeline (if any).
		if gapPresent && persistedGap.Timeline == m.Timeline {
			w.Gaps = append(w.Gaps, WindowGap{
				StartLSN:   persistedGap.GapStartLSN,
				EndLSN:     persistedGap.GapEndLSN,
				Bytes:      persistedGap.GapBytes,
				DetectedAt: persistedGap.DetectedAt,
				SlotName:   persistedGap.SlotName,
				SlotRole:   persistedGap.SlotRole,
				Source:     "gapstate",
			})
			r.Coverage.TotalGapBytes += persistedGap.GapBytes
		}
		if len(w.Gaps) > 0 || len(w.WALGapsFromManifest) > 0 {
			r.Coverage.WindowsWithGaps++
		}
		r.Windows = append(r.Windows, w)
	}

	// Newest-first ordering — operators reading "what windows do I
	// have?" want the freshest at the top.
	sort.Slice(r.Windows, func(i, j int) bool {
		return r.Windows[i].StoppedAt.After(r.Windows[j].StoppedAt)
	})

	r.Coverage.WindowCount = len(r.Windows)
	if len(r.Windows) > 0 {
		// Earliest = oldest window's StopLSN time. Latest = either
		// the latest manifest's StoppedAt or the time of the
		// newest WAL segment received (we don't have a precise
		// per-segment timestamp at this layer; use the manifest
		// commit as a lower bound for the recoverable range and
		// the operator's last WAL-status as the upper bound when
		// ingesting an upstream Coordinator state).
		oldest := r.Windows[len(r.Windows)-1]
		newest := r.Windows[0]
		r.Coverage.EarliestRecoverableTime = oldest.StoppedAt
		r.Coverage.LatestRecoverableTime = newest.StoppedAt
	}

	finish()
	return r, nil
}

// FormatLSNRange renders an LSN range as "X..Y" for the Markdown
// helper. Pure helper exposed for test code.
func FormatLSNRange(start, stop string) string {
	if start == "" && stop == "" {
		return ""
	}
	if start == "" {
		return stop
	}
	if stop == "" {
		return start
	}
	return fmt.Sprintf("%s..%s", start, stop)
}
