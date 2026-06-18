//go:build integration

// End-to-end coverage for WAL-gap detection on slot recreation.
// Issue #73 (closes the gap left by PR #69, whose corresponding test
// imported a `storage` package shape that no longer exists).
//
// What this protects against:
//
//   - The canonical Patroni-failover scenario: an agent's persistent
//     slot is dropped (failover, manual drop, slot-storm cleanup)
//     and recreated on the new leader, but the new slot's restart_lsn
//     has advanced past the agent's last-confirmed position.  Bytes
//     in that range are unrecoverable from this repo.  The system
//     MUST surface this as a structured gap result, not silently
//     bridge with the fresh restart_lsn.
//
//   - The integer-level math of GapBytes.  Off-by-one in the LSN
//     subtraction (signed vs unsigned, byte vs page units) would
//     mis-quote the gap size in operator-facing alerts.  Asserting
//     GapBytes equals (new_restart_lsn - last_confirmed_lsn) keeps
//     the contract honest.
//
//   - The first-time-bootstrap edge case: last_confirmed_lsn == 0
//     means "we've never seen this slot before"; this MUST NOT be
//     classified as a gap (every fresh agent would otherwise alert
//     on its first connection).
//
// Scope intentionally bounded:
//
//   - Drives `replication.EnsureSlot` directly, NOT the full
//     leader-follower coordinator (which adds Patroni REST polling,
//     OnEvent fan-out, gapstate.Store persistence).  EnsureSlot is
//     where the gap-detection invariant lives; the coordinator just
//     plumbs the result.  Testing the invariant in isolation keeps
//     the test wall-clock short and the failure attribution sharp.
//
// Wall-clock ≈ 10-15 s against the testkit's PG container.
package replication_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
)

// TestEnsureSlot_GapDetectedAfterDropAndAdvance is the canonical
// slot-recreate-after-failover scenario:
//
//  1. Create a slot, capture its restart_lsn as our "last confirmed"
//     position (a real agent would advance this through
//     StandbyStatusUpdate; we shortcut by reading the post-RESERVE_WAL
//     value).
//  2. Drop the slot.
//  3. Advance PG's WAL position (pg_switch_wal × N).
//  4. Call EnsureSlot with our captured LSN.
//  5. Assert: Outcome=SlotRecreated, HasGap()=true, GapBytes equals
//     the actual WAL advance.
func TestEnsureSlot_GapDetectedAfterDropAndAdvance(t *testing.T) {
	srv := testkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const slotName = "hs_gap_test"

	// Two connections: replication-mode for the slot-lifecycle ops,
	// regular-mode for SELECT / DDL.  EnsureSlot needs both because
	// CREATE_REPLICATION_SLOT requires replication mode but
	// pg_replication_slots can only be queried in regular mode.
	replConn := mustConnect(t, ctx, srv.DSN, pg.ModeReplication)
	defer replConn.Close(context.Background())
	regConn := mustConnect(t, ctx, srv.DSN, pg.ModeRegular)
	defer regConn.Close(context.Background())

	// 1. Create the slot with RESERVE_WAL so restart_lsn is populated
	//    immediately — same flag the production EnsureSlot uses on
	//    its recreate path.  Capture restart_lsn as the "we last
	//    confirmed up to here" position the failover lookup feeds.
	if err := replication.CreatePhysicalSlotReserveWAL(ctx, replConn, slotName); err != nil {
		t.Fatalf("create initial slot: %v", err)
	}
	// Best-effort cleanup if the test fails before the manual drop.
	t.Cleanup(func() {
		dropCtx, dc := context.WithTimeout(context.Background(), 10*time.Second)
		defer dc()
		_ = replication.DropSlot(dropCtx, replConn, slotName)
	})
	initial, err := replication.GetSlot(ctx, regConn, slotName)
	if err != nil {
		t.Fatalf("read initial slot: %v", err)
	}
	if initial.RestartLSN == "" {
		t.Fatalf("initial slot.restart_lsn empty after RESERVE_WAL — PG bug or wrong slot type")
	}
	lastConfirmed, err := pglogrepl.ParseLSN(initial.RestartLSN)
	if err != nil {
		t.Fatalf("parse initial restart_lsn %q: %v", initial.RestartLSN, err)
	}
	t.Logf("initial slot restart_lsn=%s (parsed=%d)", initial.RestartLSN, lastConfirmed)

	// 2. Drop the slot.  This is the moment-of-failover analogue:
	//    Patroni's reconcile drops the slot on the old leader and
	//    recreates it on the new one; the recreate races against
	//    user-driven WAL.
	if err := replication.DropSlot(ctx, replConn, slotName); err != nil {
		t.Fatalf("drop slot: %v", err)
	}

	// 3. Advance PG's WAL past last_confirmed.  Each pg_switch_wal
	//    pushes to the next segment boundary (16 MiB); a single
	//    switch is enough to make the gap non-zero, but we do
	//    three to make the size unambiguous on assertion failure.
	//    The INSERTs+CHECKPOINT ensure each segment actually has
	//    bytes, so PG isn't tempted to fast-forward through empty
	//    segments at recreate time.
	regExec(t, ctx, regConn, "CREATE TABLE gap_t (id int PRIMARY KEY, v text)")
	for i := 0; i < 3; i++ {
		regExec(t, ctx, regConn,
			fmt.Sprintf("INSERT INTO gap_t SELECT g, repeat('x', 1024) FROM generate_series(1, 200) g OFFSET %d", i*1000))
		regExec(t, ctx, regConn, "SELECT pg_switch_wal()")
	}
	regExec(t, ctx, regConn, "CHECKPOINT")

	// 4. The proof step: EnsureSlot sees the slot missing, recreates
	//    with RESERVE_WAL, and reports the gap relative to
	//    last_confirmed.
	res, err := replication.EnsureSlot(ctx, regConn, replConn, slotName, lastConfirmed)
	if err != nil {
		t.Fatalf("EnsureSlot after drop+advance: %v", err)
	}

	if res.Outcome != replication.SlotRecreated {
		t.Errorf("Outcome = %q, want %q (slot was dropped, EnsureSlot should have recreated)",
			res.Outcome, replication.SlotRecreated)
	}
	if !res.HasGap() {
		t.Errorf("HasGap() = false; expected a non-zero gap after 3× pg_switch_wal+INSERT (GapBytes=%d, GapStartLSN=%s, GapEndLSN=%s)",
			res.GapBytes, res.GapStartLSN, res.GapEndLSN)
	}
	if res.GapStartLSN != lastConfirmed {
		t.Errorf("GapStartLSN = %s, want %s (last_confirmed_lsn)",
			res.GapStartLSN, lastConfirmed)
	}
	if res.GapEndLSN <= lastConfirmed {
		t.Errorf("GapEndLSN = %s, want > %s (new restart_lsn must be past last_confirmed)",
			res.GapEndLSN, lastConfirmed)
	}
	// GapBytes contract: end - start.  Catches signed/unsigned or
	// unit (LSN-bytes vs pages) regressions.
	wantBytes := uint64(res.GapEndLSN - res.GapStartLSN)
	if res.GapBytes != wantBytes {
		t.Errorf("GapBytes = %d, want %d (end %s - start %s)",
			res.GapBytes, wantBytes, res.GapEndLSN, res.GapStartLSN)
	}
	t.Logf("gap detected: start=%s end=%s bytes=%d (~%d MiB)",
		res.GapStartLSN, res.GapEndLSN, res.GapBytes, res.GapBytes>>20)
}

// TestEnsureSlot_NoGapWhenSlotStillPresent confirms the happy path:
// if Patroni's permanent_slots brought the slot through the failover
// (Strategy A / B), EnsureSlot returns SlotFound with no gap.
func TestEnsureSlot_NoGapWhenSlotStillPresent(t *testing.T) {
	srv := testkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const slotName = "hs_no_gap_test"

	replConn := mustConnect(t, ctx, srv.DSN, pg.ModeReplication)
	defer replConn.Close(context.Background())
	regConn := mustConnect(t, ctx, srv.DSN, pg.ModeRegular)
	defer regConn.Close(context.Background())

	if err := replication.CreatePhysicalSlotReserveWAL(ctx, replConn, slotName); err != nil {
		t.Fatalf("create slot: %v", err)
	}
	t.Cleanup(func() {
		dropCtx, dc := context.WithTimeout(context.Background(), 10*time.Second)
		defer dc()
		_ = replication.DropSlot(dropCtx, replConn, slotName)
	})

	info, err := replication.GetSlot(ctx, regConn, slotName)
	if err != nil {
		t.Fatalf("read slot: %v", err)
	}
	lastConfirmed, _ := pglogrepl.ParseLSN(info.RestartLSN)

	// Slot is still present; EnsureSlot should NOT recreate.
	res, err := replication.EnsureSlot(ctx, regConn, replConn, slotName, lastConfirmed)
	if err != nil {
		t.Fatalf("EnsureSlot (slot present): %v", err)
	}
	if res.Outcome != replication.SlotFound {
		t.Errorf("Outcome = %q, want %q (slot was still present)",
			res.Outcome, replication.SlotFound)
	}
	if res.HasGap() {
		t.Errorf("HasGap() = true on a present slot — that's the no-failover happy path; got GapBytes=%d",
			res.GapBytes)
	}
}

// TestEnsureSlot_FirstTimeBootstrapNoGap confirms the bootstrap edge:
// a brand-new agent with last_confirmed_lsn == 0 calling EnsureSlot
// against a missing slot recreates it cleanly and reports no gap.
// Without this contract every fresh agent would alert "WAL gap
// detected" on its first connection, which is noise.
func TestEnsureSlot_FirstTimeBootstrapNoGap(t *testing.T) {
	srv := testkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const slotName = "hs_bootstrap_test"

	replConn := mustConnect(t, ctx, srv.DSN, pg.ModeReplication)
	defer replConn.Close(context.Background())
	regConn := mustConnect(t, ctx, srv.DSN, pg.ModeRegular)
	defer regConn.Close(context.Background())

	t.Cleanup(func() {
		dropCtx, dc := context.WithTimeout(context.Background(), 10*time.Second)
		defer dc()
		_ = replication.DropSlot(dropCtx, replConn, slotName)
	})

	// Slot doesn't exist yet; pass last_confirmed_lsn == 0 (no
	// prior position).  EnsureSlot must create + return outcome
	// SlotRecreated, but GapBytes must be 0.
	res, err := replication.EnsureSlot(ctx, regConn, replConn, slotName, pglogrepl.LSN(0))
	if err != nil {
		t.Fatalf("EnsureSlot (bootstrap): %v", err)
	}
	if res.Outcome != replication.SlotRecreated {
		t.Errorf("Outcome = %q, want %q (bootstrap creates the slot)",
			res.Outcome, replication.SlotRecreated)
	}
	if res.HasGap() {
		t.Errorf("HasGap() = true on first-time bootstrap; expected GapBytes==0, got %d",
			res.GapBytes)
	}
}

// mustConnect wraps pg.Connect with t.Fatalf on error and a
// timeout-aware ctx.  Keeps the three test functions readable.
func mustConnect(t *testing.T, ctx context.Context, dsn string, mode pg.Mode) *pg.Conn {
	t.Helper()
	c, err := pg.Connect(ctx, dsn, mode)
	if err != nil {
		t.Fatalf("pg.Connect (%s): %v", mode, err)
	}
	return c
}

// regExec runs a single SQL statement on a regular-mode connection.
// Uses pgconn directly because *pg.Conn doesn't expose an Exec; the
// regular-mode pg.Conn carries a pgconn under the hood.
func regExec(t *testing.T, ctx context.Context, c *pg.Conn, sql string) {
	t.Helper()
	r := c.PgConn().ExecParams(ctx, sql, nil, nil, nil, nil)
	if _, err := r.Close(); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
