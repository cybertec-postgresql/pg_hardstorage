//go:build !mutation_undelete_wal_unchecked

package cli

// backup_undelete_walgap.go — resurrection re-checks the WAL, not just
// the chunks.
//
// The composition that forced this (retention hunt, pass 7): a
// tombstoned backup does not hold the WAL-prune frontier — that is the
// point of retention — so `wal prune --apply` legitimately deletes the
// archived segments right after the backup's stop. `backup undelete`
// then resurrects the backup after verifying its CHUNKS at the
// visibility point, and hands back a backup that restores and boots
// perfectly — but whose `--to-latest`, standby, or time-target restore
// replays the bundled WAL, asks restore_command for the next segment,
// and gets "not in repo". PostgreSQL cannot distinguish that hole from
// the genuine end of the archive: a one-shot restore PROMOTES silently
// behind, a standby freezes forever waiting for a segment that will
// never come. Pruning leaves no gap record, so the restore-side
// refusals (which key on recorded gaps) never fire; the only signal
// was a Warning-severity contiguity event at restore time.
//
// The fix is the same shape as the pre-stream gap: record the missing
// window at the moment it becomes operationally real — resurrection —
// and let the existing preflight machinery refuse doomed restores with
// full precision. Restores from seeds whose stop is at or above the
// window's end are untouched (the seed-reachability bound), and
// --skip-gap-check remains the eyes-open override.

import (
	"context"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/inventory"
)

// recordResurrectedWALGap probes forward WAL coverage for a freshly
// resurrected backup and persists a gap record when the window after
// its stop was pruned while it was tombstoned. Returns the recorded
// window as "start..end" for the result body, or "" when coverage is
// intact (or unknowable — an empty archive is the target-reachability
// checks' concern, not a recordable hole).
//
// Best-effort but never silent: persistence failure emits CRITICAL,
// because a lost record is a restore that will later truncate
// silently. Idempotent via a same-range dedup — re-running undelete on
// an already-live backup records nothing, and a repeated
// tombstone/resurrect cycle re-records the same window only once.
func recordResurrectedWALGap(ctx context.Context, d *output.Dispatcher, sp storage.StoragePlugin, deployment, backupID string, m *backup.Manifest) string {
	if m == nil || m.StopLSN == "" {
		return ""
	}
	stop, perr := pglogrepl.ParseLSN(m.StopLSN)
	if perr != nil || stop == 0 {
		return ""
	}
	frontier, found, ferr := inventory.HighestArchivedLSN(ctx, sp, deployment, m.Timeline)
	if ferr != nil || !found || frontier <= stop {
		// No archived WAL past the stop: there is nothing to replay
		// forward INTO, so there is no hole to record — recovery ends
		// at the archive's true end, which is PG's honest semantics.
		return ""
	}
	// frontier is the END of the highest segment (exclusive); step back
	// to the last byte the archive holds — the same off-by-one the
	// contiguity preflight documents.
	hole, holeFound, herr := inventory.FirstWALHoleInRange(ctx, sp, deployment, m.Timeline, stop, frontier-1)
	if herr != nil || !holeFound {
		return "" // coverage intact: the janitors kept this backup's window
	}
	resume, resumeFound, rerr := inventory.NextArchivedLSNAtOrAfter(ctx, sp, deployment, m.Timeline, hole)
	if rerr != nil || !resumeFound {
		// A hole below the frontier always has archived WAL above it;
		// failing to find the resume point is a transient read fault.
		// Fall back to the frontier: over-recording the window is
		// conservative (refuses more), never lossy.
		resume = frontier
	}

	gs := gapstate.New(sp)
	if recs, lerr := gs.List(ctx, deployment); lerr == nil {
		for _, r := range recs {
			if r.GapStartLSN == hole.String() && r.GapEndLSN == resume.String() {
				return r.GapStartLSN + ".." + r.GapEndLSN // already recorded
			}
		}
	}
	rec := gapstate.Record{
		Deployment:  deployment,
		SlotName:    "backup-undelete",
		SlotRole:    "resurrection",
		Timeline:    m.Timeline,
		GapStartLSN: hole.String(),
		GapEndLSN:   resume.String(),
		GapBytes:    uint64(resume - hole),
		DetectedAt:  time.Now().UTC(),
	}
	sev := output.SeverityCritical
	var persistErr string
	if _, err := gs.Put(ctx, rec); err != nil {
		persistErr = err.Error()
	} else {
		sev = output.SeverityWarning
	}
	body := map[string]any{
		"backup_id":     backupID,
		"gap_start_lsn": rec.GapStartLSN,
		"gap_end_lsn":   rec.GapEndLSN,
		"gap_bytes":     rec.GapBytes,
		"message": "resurrected backup " + backupID + " stops at " + m.StopLSN + ", but the " +
			"archived WAL from " + rec.GapStartLSN + " to " + rec.GapEndLSN + " was pruned " +
			"while it was deleted (a tombstoned backup does not hold the prune frontier). " +
			"The backup itself restores fine; recovery PAST its own window cannot cross the " +
			"pruned WAL and is refused via the recorded gap. Restore bounded within the " +
			"backup's window, or use a backup taken after " + rec.GapEndLSN + ".",
	}
	if persistErr != "" {
		body["persist_error"] = persistErr
		body["message"] = body["message"].(string) +
			" RECORDING THE GAP FAILED — restores will NOT be refused automatically; " +
			"note the range by hand and fix the repository write path."
	}
	_ = d.Event(ctx, output.NewEvent(sev, "backup.undelete", "resurrected_wal_gap").
		WithSubject(output.Subject{Deployment: deployment, BackupID: backupID, Timeline: m.Timeline, LSN: rec.GapStartLSN}).
		WithBody(body))
	if persistErr != "" {
		return ""
	}
	return rec.GapStartLSN + ".." + rec.GapEndLSN
}
