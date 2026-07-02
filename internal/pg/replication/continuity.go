// continuity.go — EnsureSlot: slot found/recreated/gap-detected outcomes across Patroni failover.
package replication

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

// SlotContinuityOutcome reports what EnsureSlot did + observed.
type SlotContinuityOutcome string

const (
	// SlotFound: the slot existed on the target server. No
	// recreation, no gap. The leader-follow loop continues
	// streaming from the slot's existing restart_lsn.
	SlotFound SlotContinuityOutcome = "found"

	// SlotRecreated: the slot was missing on the target server
	// (typical after a Patroni failover where the old leader's
	// slot wasn't propagated). EnsureSlot ran
	// CREATE_REPLICATION_SLOT ... RESERVE_WAL on the new
	// leader; the new slot's restart_lsn is the one PG was
	// at when the recreate landed. Compute Gap from the
	// caller's last_confirmed_lsn.
	SlotRecreated SlotContinuityOutcome = "recreated"
)

// SlotContinuityResult is what EnsureSlot returns.
//
// GapBytes is positive when the slot's restart_lsn is AHEAD of the
// caller's last_confirmed_lsn (i.e., PG has advanced past the point we
// last acknowledged) — for BOTH outcomes:
//   - SlotRecreated: the freshly-created slot's restart_lsn is ahead
//     of last-confirmed; OR
//   - SlotFound: an existing/synced/Patroni-recreated slot is already
//     present on the new leader but its restart_lsn is ahead of
//     last-confirmed (a slot recreated at promotion by Patroni MASKS
//     the hole if we don't compare).
//
// Either case is a genuine WAL hole — PITR across the gap is
// impossible from this repo alone. The leader-follow loop emits a
// wal_gap_detected alert on Gap > 0.
//
// GapBytes is zero when:
//   - restart_lsn ≤ last_confirmed_lsn (we acknowledged at/past PG's
//     restart_lsn — no gap); OR
//   - last_confirmed_lsn is 0 (fresh follower, gap undefined); OR
//   - the slot's restart_lsn is empty (never used).
type SlotContinuityResult struct {
	Outcome          SlotContinuityOutcome
	Slot             *SlotInfo
	GapBytes         uint64
	GapStartLSN      pglogrepl.LSN // last LSN we confirmed; zero if caller passed 0
	GapEndLSN        pglogrepl.LSN // new slot's restart_lsn after recreation
	LastConfirmedLSN pglogrepl.LSN // echoed back from input
}

// EnsureSlot finds-or-creates the named slot on the target server
// and computes any WAL gap implied by recreation. Used by the
// leader-follow loop after a Patroni leader change.
//
// Connections:
//   - regConn (regular mode) is used to query pg_replication_slots
//   - replConn (replication mode) is used for CREATE_REPLICATION_SLOT
//
// lastConfirmedLSN is the highest LSN the caller has previously
// acknowledged as durably stored. Pass zero if the caller has no
// prior position (first connection on a fresh follower) — Gap is
// then meaningless and we skip the calculation.
//
// Strategy mapping (per plan §Patroni Mechanism 2):
//
//   - Strategy A (Patroni permanent_slots): the slot exists on
//     every node, including the new leader. EnsureSlot returns
//     Outcome=SlotFound. Gap is USUALLY 0 — but a slot that Patroni
//     (re)created at promotion can carry a restart_lsn ahead of
//     last_confirmed_lsn, so EnsureSlot compares and reports the gap
//     even on SlotFound. The leader-follow loop resumes streaming via
//     START_REPLICATION at last_confirmed_lsn only when Gap==0.
//
//   - Strategy C (recreate-on-detection fallback): the slot
//     doesn't exist on the new leader. EnsureSlot runs
//     CREATE_REPLICATION_SLOT ... RESERVE_WAL and returns
//     Outcome=SlotRecreated with the gap (potentially zero if
//     PG hasn't advanced past last_confirmed).
//
// Strategy B (PG 17 synced slots) is detected at the
// Strategy A path — synced slots ARE present on the new leader,
// so they look identical to Strategy A from EnsureSlot's
// perspective.
func EnsureSlot(ctx context.Context, regConn, replConn *pg.Conn, slotName string, lastConfirmedLSN pglogrepl.LSN) (*SlotContinuityResult, error) {
	if regConn == nil {
		return nil, errors.New("replication: regConn (regular mode) is required")
	}
	if replConn == nil {
		return nil, errors.New("replication: replConn (replication mode) is required")
	}
	if slotName == "" {
		return nil, errors.New("replication: slot name is required")
	}

	// Strategy A / B: slot is present on the new leader.
	info, err := GetSlot(ctx, regConn, slotName)
	if err == nil {
		res := &SlotContinuityResult{
			Outcome:          SlotFound,
			Slot:             info,
			LastConfirmedLSN: lastConfirmedLSN,
		}
		// DATA-INTEGRITY: a found slot is NOT automatically gap-free.
		// Patroni (permanent_slots / synced-slots / a recreate-on-
		// promotion race) can present a slot on the new leader whose
		// restart_lsn is AHEAD of the LSN we last archived — PG advanced
		// past our last-confirmed position and recycled the intervening
		// WAL. That is a genuine WAL hole, identical in effect to the
		// recreate path, and must be reported so the leader-follow loop
		// raises wal_gap_detected / sets gapstate and restore preflight
		// can refuse. Compute the gap here exactly as the recreate path
		// below does. When restart_lsn is empty (never used) or
		// lastConfirmedLSN is 0 (fresh follower) there's nothing to
		// compare — leave Gap zero.
		if err := populateGap(res, info, lastConfirmedLSN); err != nil {
			return res, err
		}
		return res, nil
	}
	if !errors.Is(err, ErrSlotMissing) {
		return nil, fmt.Errorf("replication: probe slot %q: %w", slotName, err)
	}

	// Strategy C: slot missing → recreate with RESERVE_WAL so the
	// new restart_lsn is populated immediately.
	if err := CreatePhysicalSlotReserveWAL(ctx, replConn, slotName); err != nil {
		return nil, fmt.Errorf("replication: recreate slot %q on new leader: %w", slotName, err)
	}

	// Re-read so we have the freshly-allocated restart_lsn.
	info, err = GetSlot(ctx, regConn, slotName)
	if err != nil {
		// We just successfully created it; if a re-read can't
		// find it the cluster is doing something pathological
		// (concurrent DROP_REPLICATION_SLOT?). Surface loudly.
		return nil, fmt.Errorf("replication: slot %q vanished immediately after recreation: %w",
			slotName, err)
	}

	res := &SlotContinuityResult{
		Outcome:          SlotRecreated,
		Slot:             info,
		LastConfirmedLSN: lastConfirmedLSN,
	}

	// Compute the gap. The post-RESERVE_WAL slot always has a
	// populated restart_lsn; an empty value on the recreate path is a
	// PG bug or a wrong-mode call (logical slot → confirmed_flush_lsn
	// instead) — surface explicitly rather than silently reporting no
	// gap.
	if info.RestartLSN == "" {
		return res, fmt.Errorf("replication: slot %q has empty restart_lsn after RESERVE_WAL recreation (this should not happen on PG 15+)",
			slotName)
	}
	if err := populateGap(res, info, lastConfirmedLSN); err != nil {
		return res, err
	}
	return res, nil
}

// populateGap fills res's Gap* fields from the slot's restart_lsn and
// the caller's lastConfirmedLSN. Shared by BOTH the SlotFound and the
// SlotRecreated paths so a found slot whose restart_lsn is ahead of
// last-confirmed is reported identically to a recreated one (a genuine
// WAL hole either way — see EnsureSlot).
//
//   - Empty restart_lsn: nothing to compare (the slot has never been
//     used). Leave Gap zero and return nil. The recreate path treats
//     an empty restart_lsn as an error separately, before calling here.
//   - lastConfirmedLSN == 0: no prior position (fresh follower / first
//     connection on this slot). Gap is undefined — leave zero.
//   - restart_lsn > lastConfirmedLSN: a real gap. GapBytes is the
//     byte distance; GapStart/GapEnd bracket the hole.
//   - restart_lsn <= lastConfirmedLSN: we've confirmed at/past PG's
//     restart position; no gap.
func populateGap(res *SlotContinuityResult, info *SlotInfo, lastConfirmedLSN pglogrepl.LSN) error {
	if info == nil || info.RestartLSN == "" {
		return nil
	}
	// info.RestartLSN is a string ("0/3000028" form). Parse to
	// pglogrepl.LSN for arithmetic.
	end, err := pglogrepl.ParseLSN(info.RestartLSN)
	if err != nil {
		return fmt.Errorf("replication: parse restart_lsn %q: %w", info.RestartLSN, err)
	}
	res.GapEndLSN = end
	res.GapStartLSN = lastConfirmedLSN
	if lastConfirmedLSN == 0 {
		// No prior position — gap is undefined, leave at zero.
		return nil
	}
	if end > lastConfirmedLSN {
		res.GapBytes = uint64(end - lastConfirmedLSN)
	}
	// end <= lastConfirmedLSN: we're past PG's current position; no gap.
	return nil
}

// HasGap reports whether the result indicates a non-zero WAL gap.
// Convenience for the leader-follow loop's alert-fan-out logic.
func (r *SlotContinuityResult) HasGap() bool {
	return r != nil && r.GapBytes > 0
}
