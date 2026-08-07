package cli

// wal_prestream_gap.go — the WAL nobody will ever archive, recorded so
// a restore can refuse instead of silently truncating.
//
// Found by the chaos gate's first boot-proof run. The operator flow
// `init --quick` (or any backup) followed by starting `wal stream`
// leaves a window nothing covers: the backup bundles WAL to its own
// stop, and the fresh replication slot anchors at the position PG
// holds WHEN THE SLOT IS CREATED — later. Everything between was never
// archived and never will be.
//
// What makes it dangerous is how it fails. A `--to-latest` (or
// standby) recovery from that backup replays the bundled WAL, asks
// restore_command for the next segment, gets "not in repo" — and PG
// cannot distinguish a hole from the genuine end of the archive, so it
// ends recovery, PROMOTES, and reports success at a state that may be
// arbitrarily far behind. `wal audit` is blind too: findGaps sees
// holes BETWEEN archived segments, and this hole ends where archived
// WAL begins. Measured: a soak backup promoted cleanly missing the
// entire fault-window workload.
//
// The stream cannot heal the window (PG has recycled that WAL — the
// same reasoning as wal.start_before_slot_restart_lsn). What it CAN do
// is what the Patroni coordinator does for failover gaps: persist a
// gapstate record so the restore-side preflight refuses a recovery
// that would cross it, and say so loudly now.

import (
	"context"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

// recordPreStreamGap runs when a stream starts on a FRESH slot — the
// only resume strategy that can begin past existing backups. It finds
// the oldest live backup whose stop precedes the stream start and
// persists the uncovered window [minStop, startLSN) as a gap record.
//
// Best-effort but never silent: persistence failure emits CRITICAL,
// because a lost record is a restore that will later truncate
// silently. Idempotent across restarts via a same-range dedup — a
// crash-loop before the first segment commits must not pile up
// records.
func recordPreStreamGap(ctx context.Context, d *output.Dispatcher, sp storage.StoragePlugin, opts walStreamOptions, timeline uint32, startLSN pglogrepl.LSN) {
	if startLSN == 0 {
		return
	}
	var minStop pglogrepl.LSN
	found := false
	for m, lerr := range backup.NewManifestStore(sp).ListAttestationless(ctx, opts.deployment) {
		if lerr != nil || m == nil || m.StopLSN == "" {
			continue
		}
		stop, perr := pglogrepl.ParseLSN(m.StopLSN)
		if perr != nil {
			continue
		}
		if !found || stop < minStop {
			minStop, found = stop, true
		}
	}
	if !found || minStop >= startLSN {
		return // no backup predates the stream: nothing is uncovered
	}
	// The window opens where COVERAGE ends — the archive frontier when
	// one exists (the Patroni-failover fresh slot: months of WAL are
	// archived, only [frontier, start) is missing), else the oldest
	// backup's stop (the first-ever stream: nothing is archived yet).
	gapStart := preStreamGapStart(ctx, sp, opts.deployment, timeline, minStop)
	if gapStart >= startLSN {
		return // the archive reaches the anchor: nothing is uncovered
	}

	store := gapstate.New(sp)
	if recs, lerr := store.List(ctx, opts.deployment); lerr == nil {
		for _, r := range recs {
			if r.GapStartLSN == gapStart.String() && r.GapEndLSN == startLSN.String() {
				return // already recorded (agent restart before first commit)
			}
		}
	}
	rec := gapstate.Record{
		Deployment:  opts.deployment,
		SlotName:    opts.slotName,
		Timeline:    timeline,
		GapStartLSN: gapStart.String(),
		GapEndLSN:   startLSN.String(),
		GapBytes:    uint64(startLSN - gapStart),
		DetectedAt:  time.Now().UTC(),
	}
	sev := output.SeverityCritical
	var persistErr string
	if _, err := store.Put(ctx, rec); err != nil {
		persistErr = err.Error()
	} else {
		sev = output.SeverityWarning
	}
	body := map[string]any{
		"gap_start_lsn": rec.GapStartLSN,
		"gap_end_lsn":   rec.GapEndLSN,
		"gap_bytes":     rec.GapBytes,
		"message": "this stream starts on a FRESH slot at " + startLSN.String() + ", but " +
			"coverage (archive or backup bundle) ends at " + gapStart.String() + ": the WAL " +
			"between was never archived and never will be (PG has moved past it). " +
			"Point-in-time recovery from backups older than this stream start cannot cross " +
			"the window — restores that would are refused via the recorded gap. Take a " +
			"fresh backup to re-anchor PITR.",
	}
	if persistErr != "" {
		body["persist_error"] = persistErr
		body["message"] = body["message"].(string) +
			" RECORDING THE GAP FAILED — restores will NOT be refused automatically; " +
			"note the range by hand and fix the repository write path."
	}
	_ = d.Event(ctx, output.NewEvent(sev, "wal.stream", "pre_stream_gap").
		WithSubject(output.Subject{Deployment: opts.deployment, Timeline: timeline, LSN: rec.GapEndLSN}).
		WithBody(body))
}
