// gapcheck.go — preflightWALGap: refuses PITR when target LSN falls inside a persisted WAL gap.
package restore

import (
	"context"
	"fmt"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/inventory"
)

// preflightWALContiguity is a PHYSICAL, warning-only completeness check:
// it looks for a WAL segment that is MISSING from the archive between the
// backup's stop point and an LSN recovery target, on the backup's
// timeline. Unlike preflightWALGap — which consults the RECORDED gap
// state — this scans the segments actually present, so it surfaces holes
// from a pruning bug, storage corruption, or a manual deletion that no
// gap record describes: exactly the cases where PG would otherwise halt
// mid-recovery at the missing segment with no up-front signal.
//
// It only WARNS, never refuses. Segment-presence inference cannot account
// for every PITR shape (cross-timeline replay, WAL shipped inside the
// base backup's pg_wal, an operator deliberately restoring only to
// consistency), so a false positive must never block a legitimate
// restore. The warning is a heads-up to investigate before recovery
// stalls.
//
// Scope: LSN targets only — TargetTime/Name/latest can't be resolved to a
// segment range pre-flight (the same limitation as preflightWALGap).
func preflightWALContiguity(ctx context.Context, sp storage.StoragePlugin, deployment string, m *backup.Manifest, recovery *Recovery, emit func(*output.Event)) {
	if recovery == nil || !recovery.Enable || recovery.SkipGapCheck || recovery.TargetLSN == "" || m == nil {
		return
	}
	target, err := pglogrepl.ParseLSN(recovery.TargetLSN)
	if err != nil {
		return // a malformed target is surfaced by preflightWALGap's refusal path
	}
	stop, err := pglogrepl.ParseLSN(m.StopLSN)
	if err != nil {
		return
	}
	if target < stop {
		return // an unreachable (too-early) target is CheckTargetReachable's job
	}
	hole, found, herr := inventory.FirstWALHoleInRange(ctx, sp, deployment, m.Timeline, stop, target)
	if herr != nil || !found {
		return // probe failure degrades silently; no hole → nothing to warn about
	}
	if emit == nil {
		return
	}
	emit(output.NewEvent(output.SeverityWarning, "restore", "wal_archive_hole").
		WithSubject(output.Subject{Deployment: deployment, Timeline: m.Timeline, LSN: hole.String()}).
		WithBody(map[string]any{
			"deployment":       deployment,
			"timeline":         m.Timeline,
			"missing_from_lsn": hole.String(),
			"backup_stop_lsn":  m.StopLSN,
			"target_lsn":       recovery.TargetLSN,
		}).
		WithSuggestion(&output.Suggestion{
			Human: "a WAL segment needed to replay from this backup to the target is MISSING from the archive, and no gap record describes it (likely pruning, corruption, or manual deletion). Recovery will HALT at this LSN instead of reaching the target. Inspect with `pg_hardstorage wal list --repo <repo> " + deployment + "`; restore from a later backup, or pick a target before the hole, if the WAL cannot be recovered.",
		}))
}

// preflightWALGap consults the deployment's persisted WAL-gap
// state and refuses the restore when the operator's PITR
// target falls within a known gap. The plan calls this out:
// "Backup taken after this point will note the gap so PITR is
// refused"; the agent records gaps via the leader-follow
// Coordinator, this helper is the consultation half.
//
// Scope today:
//
//   - TargetLSN set: parse + check against every persisted
//     gap. Refuse when target ∈ [gap_start, gap_end). The
//     half-open range matches PG's exclusive-end convention
//     for WAL ranges.
//   - TargetTime / TargetName / no target / StandbyMode: not
//     gap-checkable today. We don't know the LSN until PG
//     resolves it, so we can't refuse pre-emptively. The
//     agent's `wal_gap_persistent` doctor issue + the
//     emitted `wal_gap_detected` events stay the operator's
//     primary signal in those cases.
//
// On a known-bad target the function returns a structured
// `restore.target_in_wal_gap` error with a Suggestion pointing
// at `pg_hardstorage wal gaps <deployment>` for inspection +
// `pg_hardstorage repair slot <deployment>` for the underlying
// slot remediation. The CLI exit-mapper routes
// `restore.target_in_wal_gap` to ExitError; future iterations
// might prefer ExitConflict.
//
// Failure modes:
//
//   - gapstate.List error: surface as a warning (same shape
//     as doctor's wal.gap_state_unreadable code) but DON'T
//     refuse — a transient List error shouldn't take down a
//     legitimate restore. The operator's pre-flight via
//     doctor would have caught a persistent corruption.
//   - LSN parse error: refuse with usage.bad_target_lsn.
func preflightWALGap(ctx context.Context, sp storage.StoragePlugin, deployment string, recovery *Recovery, manifestGaps []backup.WALGap, emit func(*output.Event)) error {
	if recovery == nil || !recovery.Enable {
		return nil
	}
	// SkipGapCheck is the operator's explicit override. We
	// short-circuit BOTH the LSN refusal AND the time-target
	// advisory paths so a single flag flip silences the entire
	// gap-pre-flight surface. The override is recorded as a
	// Notice event so post-incident review sees the choice was
	// made (the agent's gap-detection events still fire on the
	// underlying detection — this just says "operator
	// acknowledged and proceeded anyway").
	if recovery.SkipGapCheck {
		if emit != nil {
			emit(output.NewEvent(output.SeverityNotice, "restore", "wal_gap_check_skipped").
				WithSubject(output.Subject{Deployment: deployment}).
				WithBody(map[string]any{
					"hint": "the+ WAL-gap pre-flight was bypassed via --skip-gap-check; the operator accepted the risk that the PITR target may land in a known gap",
				}))
		}
		return nil
	}
	if recovery.TargetLSN == "" {
		// Time-targeted / name-targeted / no-target paths can't be
		// statically refused by LSN comparison.  Pre-v23 audit, we
		// emitted a warning and proceeded — the audit's #1 case
		// pointed out that a time-target landing inside a gap then
		// produced "undefined behaviour" (PG would bail at recovery
		// time after consuming substantial wallclock).
		//
		// New posture: when ANY recorded gap exists and the
		// operator has set a time/name target, REFUSE by default.
		// The operator can override with --skip-gap-check (handled
		// above) when they explicitly accept the risk.  No-target
		// (end-of-WAL) restores still warn rather than refuse —
		// PG's own end-of-WAL semantics handle the gap-tail
		// scenario correctly even when the agent missed bytes.
		if recovery.IsTargetSet() {
			if err := preflightTimeTargetGap(ctx, sp, deployment, recovery, manifestGaps); err != nil {
				return err
			}
			if emit != nil {
				emitTimeTargetGapWarning(ctx, sp, deployment, recovery, manifestGaps, emit)
			}
		}
		return nil
	}
	target, err := pglogrepl.ParseLSN(recovery.TargetLSN)
	if err != nil {
		return output.NewError("usage.bad_target_lsn",
			fmt.Sprintf("restore: parse target_lsn %q: %v", recovery.TargetLSN, err)).
			Wrap(output.ErrUsage)
	}

	// Two sources for gap records, both consulted:
	//
	//   1. manifestGaps — embedded on the manifest at backup
	//      commit time; signed via the manifest's attestation;
	//      survives gapstate GC + cross-region replication.
	//      First-class source.
	//   2. live gapstate — agent's continuously-updated record
	//      under wal/<deployment>/gaps/. Catches gaps detected
	//      AFTER the backup was taken (still a concern for
	//      LSNs in the post-commit window).
	//
	// Either source refusing is enough. We don't try to dedupe;
	// the same gap recorded in both produces ONE refusal, not
	// two, because the loop short-circuits on the first match.
	for _, g := range manifestGaps {
		if err := checkOneGap(target, g.GapStartLSN, g.GapEndLSN, g.GapBytes,
			g.DetectedAt.UTC().Format("2006-01-02T15:04:05Z"),
			g.SlotName, g.Timeline, deployment, "manifest"); err != nil {
			return err
		}
	}

	gaps, err := gapstate.New(sp).List(ctx, deployment)
	if err != nil {
		// Best-effort: surface as a warning event but proceed
		// with the restore. The manifest already had a chance
		// to refuse via embedded gaps; if it didn't and live
		// gapstate is unreadable, we degrade rather than block.
		if emit != nil {
			emit(output.NewEvent(output.SeverityWarning, "restore", "gap_state_unreadable").
				WithSubject(output.Subject{Deployment: deployment}).
				WithBody(map[string]any{"error": err.Error()}))
		}
		return nil
	}

	for _, g := range gaps {
		if err := checkOneGap(target, g.GapStartLSN, g.GapEndLSN, g.GapBytes,
			g.DetectedAt.UTC().Format("2006-01-02T15:04:05Z"),
			g.SlotName, g.Timeline, deployment, "live"); err != nil {
			return err
		}
	}
	return nil
}

// checkOneGap is the per-record refusal logic, shared between
// the manifest-embedded gaps and live gapstate gaps. Returns
// nil when the target falls outside the gap (allow) or the
// record is malformed (silent skip); returns a structured
// error when the target falls in [start, end) (refuse).
//
// `source` is "manifest" or "live"; it's surfaced in the
// error message so the operator knows which record fired the
// refusal (e.g., a stale manifest-embedded gap can be
// distinguished from a fresh live one).
func checkOneGap(target pglogrepl.LSN, startStr, endStr string, bytes uint64, detectedAt, slotName string, tli uint32, deployment, source string) error {
	start, sErr := pglogrepl.ParseLSN(startStr)
	if sErr != nil {
		return nil // malformed; skip
	}
	end, eErr := pglogrepl.ParseLSN(endStr)
	if eErr != nil {
		return nil
	}
	if target < start || target >= end {
		return nil
	}
	return output.NewError("restore.target_in_wal_gap",
		fmt.Sprintf("restore: target_lsn %s falls within a known WAL gap (%s..%s, %d bytes, detected %s on slot %q TLI %d, source=%s) — PITR within this range is impossible from this repo",
			target.String(), startStr, endStr, bytes, detectedAt, slotName, tli, source)).
		WithSuggestion(&output.Suggestion{
			Human:   "the agent recorded a Patroni-failover gap that covers the operator's target LSN. Use `pg_hardstorage wal gaps <deployment>` to inspect the full record history. Pick a target LSN OUTSIDE the gap: either BELOW gap_start (keeps everything before the gap; gap_start itself is refused because a WAL record can span the segment boundary into the missing range) or AT/ABOVE gap_end (resumes after the gap). The underlying slot issue is investigated via `pg_hardstorage repair slot <deployment>`.",
			Command: "pg_hardstorage wal gaps " + deployment,
			DocURL:  "https://docs.pghardstorage.org/runbooks/wal-gap-detected",
		})
}

// preflightTimeTargetGap refuses a time/name-targeted PITR when
// any recorded gap exists for the deployment.  a
// time target landing inside a gap previously emitted only an
// advisory event and let the restore proceed; PG would then bail
// out of recovery after substantial wallclock.  We can't
// statically check time → LSN without PG running, so the safe
// default is to refuse outright when gaps exist.  Operators
// override with --skip-gap-check (handled by the caller).
//
// Returns nil when no gaps exist (clean deployment); returns a
// structured `restore.target_in_wal_gap` error otherwise.  Best-
// effort live-gapstate read: a List failure falls back to
// manifest-only gaps so a transient backend issue doesn't block
// a legitimate restore.
func preflightTimeTargetGap(ctx context.Context, sp storage.StoragePlugin, deployment string, recovery *Recovery, manifestGaps []backup.WALGap) error {
	liveGaps, _ := gapstate.New(sp).List(ctx, deployment) // best-effort
	totalGaps := len(manifestGaps) + len(liveGaps)
	if totalGaps == 0 {
		return nil
	}
	var targetDescr string
	switch {
	case !recovery.TargetTime.IsZero():
		targetDescr = "target_time " + recovery.TargetTime.UTC().Format("2006-01-02T15:04:05Z07:00")
	case recovery.TargetName != "":
		targetDescr = fmt.Sprintf("target_name %q", recovery.TargetName)
	default:
		// IsTargetSet() is true here, so one of these branches must apply.
		targetDescr = "target"
	}
	return output.NewError("restore.target_in_wal_gap",
		fmt.Sprintf("restore: %s cannot be statically gap-checked and the deployment has %d recorded WAL gap(s) (%d in manifest, %d live) — refusing rather than risk a PITR that bails out of recovery after PG resolves the target to an LSN inside a gap",
			targetDescr, totalGaps, len(manifestGaps), len(liveGaps))).
		WithSuggestion(&output.Suggestion{
			Human:   "either re-target with --to-lsn (a static check then runs and either passes or refuses precisely), inspect `pg_hardstorage wal gaps <deployment>` to confirm your time/name target falls outside the gap windows, or pass --skip-gap-check to acknowledge the risk and proceed (the post-mortem audit will record the bypass).",
			Command: "pg_hardstorage wal gaps " + deployment,
			DocURL:  "https://docs.pghardstorage.org/runbooks/wal-gap-detected",
		})
}

// emitTimeTargetGapWarning surfaces a Warning event when a
// time-targeted (or name-targeted) PITR is run against a
// deployment that has any recorded gaps. We can't refuse
// statically — the LSN of TargetTime / TargetName is unknown
// until PG resolves it during recovery — but a silent allow
// would mean the operator might run a PITR whose target
// happens to land inside a gap and only discover the failure
// when PG bails out of recovery.
//
// The warning includes the count of recorded gaps + the
// manifest-vs-live source breakdown so the operator can
// decide whether to switch to a TargetLSN (gets the static
// check) or inspect via `wal gaps <deployment>` to confirm
// the target is outside known gap windows.
//
// Best-effort: a gapstate.List failure here is silent (the
// operator's gap-check is advisory; doctor would have already
// surfaced a persistent corruption).
func emitTimeTargetGapWarning(ctx context.Context, sp storage.StoragePlugin, deployment string, recovery *Recovery, manifestGaps []backup.WALGap, emit func(*output.Event)) {
	liveGaps, _ := gapstate.New(sp).List(ctx, deployment) // best-effort
	totalGaps := len(manifestGaps) + len(liveGaps)
	if totalGaps == 0 {
		return // clean deployment, no signal to surface
	}

	body := map[string]any{
		"manifest_gap_count": len(manifestGaps),
		"live_gap_count":     len(liveGaps),
	}
	switch {
	case !recovery.TargetTime.IsZero():
		body["target_time"] = recovery.TargetTime.UTC().Format("2006-01-02T15:04:05Z07:00")
		body["target_kind"] = "time"
	case recovery.TargetName != "":
		body["target_name"] = recovery.TargetName
		body["target_kind"] = "name"
	default:
		body["target_kind"] = "end-of-wal"
	}

	emit(output.NewEvent(output.SeverityWarning, "restore", "wal_gap_advisory").
		WithSubject(output.Subject{Deployment: deployment}).
		WithBody(body).
		WithSuggestion(&output.Suggestion{
			Human:   "this deployment has recorded WAL gaps (Patroni failovers where the agent missed bytes). PITR by time / name / end-of-WAL can't be statically gap-checked — pg_hardstorage doesn't have a LSN-vs-time index without PG running. Either switch to `--to-lsn` (gets the static check), or inspect `pg_hardstorage wal gaps <deployment>` to confirm your target falls outside the gap windows. The agent's recorded gap-detection times are usually a tight bound on when the gap WAL is missing.",
			Command: "pg_hardstorage wal gaps " + deployment,
			DocURL:  "https://docs.pghardstorage.org/runbooks/wal-gap-detected",
		}))
}
