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
// GapBytes is positive when SlotRecreated AND the new slot's
// restart_lsn is AHEAD of the caller's last_confirmed_lsn (i.e.,
// PG has advanced past the point we last acknowledged). That's a
// genuine WAL hole — PITR across the gap is impossible from this
// repo alone. The leader-follow loop emits a wal_gap_detected
// alert on Gap > 0.
//
// GapBytes is zero when:
//   - SlotFound (no recreation happened); OR
//   - SlotRecreated AND new restart_lsn ≤ last_confirmed_lsn
//     (we acknowledged further than PG's restart_lsn — no gap).
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
//     Outcome=SlotFound, Gap=0. The leader-follow loop resumes
//     streaming via START_REPLICATION at last_confirmed_lsn.
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
		return &SlotContinuityResult{
			Outcome:          SlotFound,
			Slot:             info,
			LastConfirmedLSN: lastConfirmedLSN,
		}, nil
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

	// Compute the gap.
	//
	// info.RestartLSN is a string ("0/3000028" form). Parse to
	// pglogrepl.LSN for arithmetic. The post-RESERVE_WAL slot
	// always has a populated restart_lsn; an empty value here
	// would be a PG bug or a wrong-mode call (logical slot →
	// confirmed_flush_lsn instead) — surface explicitly.
	if info.RestartLSN == "" {
		return res, fmt.Errorf("replication: slot %q has empty restart_lsn after RESERVE_WAL recreation (this should not happen on PG 15+)",
			slotName)
	}
	end, err := pglogrepl.ParseLSN(info.RestartLSN)
	if err != nil {
		return res, fmt.Errorf("replication: parse new restart_lsn %q: %w", info.RestartLSN, err)
	}
	res.GapEndLSN = end
	res.GapStartLSN = lastConfirmedLSN
	if lastConfirmedLSN == 0 {
		// No prior position — gap is undefined, leave at zero.
		// The caller treats this as "first-time bootstrap on
		// this slot" rather than a regression.
		return res, nil
	}
	if end > lastConfirmedLSN {
		res.GapBytes = uint64(end - lastConfirmedLSN)
	}
	// end <= lastConfirmedLSN: we're past PG's current position;
	// no gap. Leaving GapBytes at zero is correct.
	return res, nil
}

// HasGap reports whether the result indicates a non-zero WAL gap.
// Convenience for the leader-follow loop's alert-fan-out logic.
func (r *SlotContinuityResult) HasGap() bool {
	return r != nil && r.GapBytes > 0
}
