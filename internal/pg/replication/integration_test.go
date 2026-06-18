// Build-tagged integration tests against a real PG container.
// Run with `make test-integration` (requires Docker).
//
//go:build integration

package replication_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
)

// connectReplication opens a fresh replication-mode connection.
func connectReplication(t *testing.T, dsn string) *pg.Conn {
	t.Helper()
	c, err := pg.Connect(context.Background(), dsn, pg.ModeReplication)
	if err != nil {
		t.Fatalf("connect (replication): %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// connectRegular opens a fresh regular-mode connection (for the
// pg_replication_slots query).
func connectRegular(t *testing.T, dsn string) *pg.Conn {
	t.Helper()
	c, err := pg.Connect(context.Background(), dsn, pg.ModeRegular)
	if err != nil {
		t.Fatalf("connect (regular): %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func TestIntegration_SlotLifecycle(t *testing.T) {
	srv := testkit.StartPostgres(t)

	// Create on a replication conn.
	repl := connectReplication(t, srv.DSN)
	const slot = "hsctl_lifecycle_test"
	if err := replication.CreatePhysicalSlot(context.Background(), repl, slot); err != nil {
		t.Fatalf("create slot: %v", err)
	}
	t.Cleanup(func() {
		// Best-effort cleanup; second run-through is intentional too,
		// to exercise idempotence.
		repl2 := connectReplication(t, srv.DSN)
		_ = replication.DropSlot(context.Background(), repl2, slot)
		_ = repl2.Close(context.Background())
	})

	// Idempotent re-create must not error.
	if err := replication.CreatePhysicalSlot(context.Background(), repl, slot); err != nil {
		t.Errorf("idempotent create returned error: %v", err)
	}

	// Read via regular conn.
	reg := connectRegular(t, srv.DSN)
	info, err := replication.GetSlot(context.Background(), reg, slot)
	if err != nil {
		t.Fatalf("get slot: %v", err)
	}
	if info.Name != slot {
		t.Errorf("Name = %q, want %q", info.Name, slot)
	}
	if info.Type != "physical" {
		t.Errorf("Type = %q, want physical", info.Type)
	}
	if info.Active {
		t.Error("Active = true; expected inactive (no consumer yet)")
	}

	// Drop, then a get should miss.
	if err := replication.DropSlot(context.Background(), repl, slot); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := replication.GetSlot(context.Background(), reg, slot); !errors.Is(err, replication.ErrSlotMissing) {
		t.Errorf("after drop, GetSlot should return ErrSlotMissing; got %v", err)
	}

	// Idempotent re-drop must not error.
	if err := replication.DropSlot(context.Background(), repl, slot); err != nil {
		t.Errorf("idempotent drop: %v", err)
	}
}

// recordingSink for integration tests.
type recordingSink struct {
	count atomic.Int32
	last  atomic.Uint64
}

func (s *recordingSink) OnRecord(_ context.Context, r replication.XLogRecord) error {
	s.count.Add(1)
	if uint64(r.WALStart) > s.last.Load() {
		s.last.Store(uint64(r.WALStart))
	}
	return nil
}

func (s *recordingSink) SyncedLSN() pglogrepl.LSN {
	return pglogrepl.LSN(s.last.Load())
}

func TestIntegration_StreamReceivesXLogData(t *testing.T) {
	srv := testkit.StartPostgres(t)

	repl := connectReplication(t, srv.DSN)
	const slot = "hsctl_stream_test"
	// RESERVE_WAL so the slot has a non-NULL restart_lsn at
	// creation time.  Without it, START_REPLICATION SLOT 0/0
	// asks PG for segment 0 (LSN 0/0) which never exists on a
	// fresh cluster (initdb advances past it before the first
	// "ready to accept connections"), and the test fails with
	// "requested WAL segment 000000010000000000000000 has
	// already been removed".
	if err := replication.CreatePhysicalSlotReserveWAL(context.Background(), repl, slot); err != nil {
		t.Fatalf("create slot: %v", err)
	}
	t.Cleanup(func() {
		repl2 := connectReplication(t, srv.DSN)
		defer repl2.Close(context.Background())
		_ = replication.DropSlot(context.Background(), repl2, slot)
	})

	// Generate some WAL by issuing a CHECKPOINT (which forces a WAL
	// switch) on a regular-mode connection — the test PG cluster is
	// otherwise quiet.
	reg := connectRegular(t, srv.DSN)
	res := reg.PgConn().ExecParams(context.Background(), "CHECKPOINT", nil, nil, nil, nil).Read()
	if res.Err != nil {
		t.Fatalf("CHECKPOINT: %v", res.Err)
	}

	// Stream for a couple of seconds; we expect at least one keepalive
	// and (with luck) some XLogData. The robust test is "Stream
	// returns ctx.Canceled and we received at least one Server-side
	// message" — i.e. the protocol round-tripped at all.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Read the slot's restart_lsn and start from it explicitly.
	// pglogrepl's StartReplication interprets a zero LSN literally
	// (segment 0/0), which on a fresh PG cluster is "already
	// removed" by the time initdb finishes — surfaced as
	//   ERROR: requested WAL segment 000000010000000000000000 has
	//          already been removed
	// when the test passed the zero-valued StreamOptions.StartLSN.
	// Reading the slot's restart_lsn first is what production code
	// (internal/cli/wal.go resolveStartLSN) does too.
	slotInfo, err := replication.GetSlot(context.Background(), reg, slot)
	if err != nil {
		t.Fatalf("GetSlot before stream: %v", err)
	}
	startLSN, err := pglogrepl.ParseLSN(slotInfo.RestartLSN)
	if err != nil {
		t.Fatalf("parse slot RestartLSN %q: %v", slotInfo.RestartLSN, err)
	}

	sink := &recordingSink{}
	// Stream takes ownership of repl via Hijack; remove from cleanup.
	streamRepl := connectReplication(t, srv.DSN)
	go func() {
		// Generate WAL while streaming so we have content.
		time.Sleep(500 * time.Millisecond)
		reg2 := connectRegular(t, srv.DSN)
		defer reg2.Close(context.Background())
		_ = reg2.PgConn().ExecParams(ctx, "SELECT pg_switch_wal()", nil, nil, nil, nil).Read()
		_ = reg2.PgConn().ExecParams(ctx, "CREATE TABLE IF NOT EXISTS hsctl_test (i int)", nil, nil, nil, nil).Read()
		_ = reg2.PgConn().ExecParams(ctx, "INSERT INTO hsctl_test SELECT generate_series(1, 1000)", nil, nil, nil, nil).Read()
		_ = reg2.PgConn().ExecParams(ctx, "CHECKPOINT", nil, nil, nil, nil).Read()

		// Let some time pass so records flow.
		time.Sleep(2 * time.Second)
		cancel()
	}()

	err = replication.Stream(ctx, streamRepl, replication.StreamOptions{
		Slot:                 slot,
		StartLSN:             startLSN,
		StatusUpdateInterval: 250 * time.Millisecond,
	}, sink)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled; got %v", err)
	}

	// Allow some time for slot status to settle on the server side.
	time.Sleep(500 * time.Millisecond)

	// We don't strictly assert sink received data — under heavy load
	// PG may bundle multiple records into one keepalive — but we DO
	// assert that the slot now reports activity (restart_lsn populated).
	info, err := replication.GetSlot(context.Background(), reg, slot)
	if err != nil {
		t.Fatalf("post-stream GetSlot: %v", err)
	}
	if info.RestartLSN == "" {
		t.Errorf("RestartLSN should be populated after streaming; got %q", info.RestartLSN)
	}
	t.Logf("streamed %d records; slot.restart_lsn = %s", sink.count.Load(), info.RestartLSN)
}

func TestIntegration_Stream_RejectsRegularConn(t *testing.T) {
	srv := testkit.StartPostgres(t)
	reg := connectRegular(t, srv.DSN)
	err := replication.Stream(context.Background(), reg, replication.StreamOptions{
		Slot: "any",
	}, &recordingSink{})
	if err == nil {
		t.Error("Stream against a regular conn must error")
	}
}

func TestIntegration_CreateSlot_RejectsRegularConn(t *testing.T) {
	srv := testkit.StartPostgres(t)
	reg := connectRegular(t, srv.DSN)
	err := replication.CreatePhysicalSlot(context.Background(), reg, "x")
	if err == nil {
		t.Error("CreatePhysicalSlot against a regular conn must error")
	}
}

func TestIntegration_GetSlot_RejectsReplicationConn(t *testing.T) {
	srv := testkit.StartPostgres(t)
	repl := connectReplication(t, srv.DSN)
	_, err := replication.GetSlot(context.Background(), repl, "x")
	if err == nil {
		t.Error("GetSlot against a replication conn must error")
	}
}

// TestIntegration_CreatePhysicalSlotReserveWAL: the variant that
// adds RESERVE_WAL so restart_lsn is populated immediately after
// creation. Required by EnsureSlot's gap calculation.
func TestIntegration_CreatePhysicalSlotReserveWAL(t *testing.T) {
	srv := testkit.StartPostgres(t)

	const slot = "hsctl_reserve_wal_test"
	repl := connectReplication(t, srv.DSN)
	t.Cleanup(func() {
		repl2 := connectReplication(t, srv.DSN)
		_ = replication.DropSlot(context.Background(), repl2, slot)
		_ = repl2.Close(context.Background())
	})

	if err := replication.CreatePhysicalSlotReserveWAL(context.Background(), repl, slot); err != nil {
		t.Fatalf("CreatePhysicalSlotReserveWAL: %v", err)
	}

	reg := connectRegular(t, srv.DSN)
	info, err := replication.GetSlot(context.Background(), reg, slot)
	if err != nil {
		t.Fatalf("GetSlot: %v", err)
	}
	// The whole point of RESERVE_WAL: restart_lsn must be
	// populated immediately. With plain CreatePhysicalSlot
	// (no RESERVE_WAL) this would be empty.
	if info.RestartLSN == "" {
		t.Errorf("RESERVE_WAL slot should have a non-empty restart_lsn; got info=%+v", info)
	}

	// Idempotent on already-exists.
	if err := replication.CreatePhysicalSlotReserveWAL(context.Background(), repl, slot); err != nil {
		t.Errorf("second CreatePhysicalSlotReserveWAL should be a no-op; got %v", err)
	}
}

// TestIntegration_EnsureSlot_FoundExisting: when the slot is
// already present (the Strategy A / B happy path: Patroni
// permanent_slots / PG 17 synced slots), EnsureSlot returns
// SlotFound with no gap.
func TestIntegration_EnsureSlot_FoundExisting(t *testing.T) {
	srv := testkit.StartPostgres(t)
	const slot = "hsctl_ensure_found"

	repl := connectReplication(t, srv.DSN)
	t.Cleanup(func() {
		repl2 := connectReplication(t, srv.DSN)
		_ = replication.DropSlot(context.Background(), repl2, slot)
		_ = repl2.Close(context.Background())
	})
	if err := replication.CreatePhysicalSlotReserveWAL(context.Background(), repl, slot); err != nil {
		t.Fatalf("setup CreatePhysicalSlotReserveWAL: %v", err)
	}

	reg := connectRegular(t, srv.DSN)
	res, err := replication.EnsureSlot(context.Background(), reg, repl, slot, 0)
	if err != nil {
		t.Fatalf("EnsureSlot: %v", err)
	}
	if res.Outcome != replication.SlotFound {
		t.Errorf("Outcome = %q, want %q", res.Outcome, replication.SlotFound)
	}
	if res.GapBytes != 0 {
		t.Errorf("found-existing should report Gap=0; got %d", res.GapBytes)
	}
	if res.Slot == nil || res.Slot.Name != slot {
		t.Errorf("Slot.Name should be %q; got %+v", slot, res.Slot)
	}
}

// TestIntegration_EnsureSlot_RecreatesMissing: the Strategy C
// path. Slot is absent on the target; EnsureSlot creates it with
// RESERVE_WAL and reports SlotRecreated. Gap calculation depends
// on lastConfirmedLSN — we exercise both the "no prior position"
// and "prior position before current" cases.
func TestIntegration_EnsureSlot_RecreatesMissing(t *testing.T) {
	srv := testkit.StartPostgres(t)
	const slot = "hsctl_ensure_recreate"

	repl := connectReplication(t, srv.DSN)
	reg := connectRegular(t, srv.DSN)
	t.Cleanup(func() {
		repl2 := connectReplication(t, srv.DSN)
		_ = replication.DropSlot(context.Background(), repl2, slot)
		_ = repl2.Close(context.Background())
	})

	// Confirm the slot is absent at start.
	if _, err := replication.GetSlot(context.Background(), reg, slot); !errors.Is(err, replication.ErrSlotMissing) {
		t.Fatalf("setup: slot should be missing at start; got %v", err)
	}

	// Case 1: lastConfirmedLSN == 0 → no prior position; gap is
	// undefined (we leave at 0 and let the caller treat as
	// first-time bootstrap).
	res, err := replication.EnsureSlot(context.Background(), reg, repl, slot, 0)
	if err != nil {
		t.Fatalf("EnsureSlot: %v", err)
	}
	if res.Outcome != replication.SlotRecreated {
		t.Errorf("Outcome = %q, want %q", res.Outcome, replication.SlotRecreated)
	}
	if res.GapBytes != 0 {
		t.Errorf("first-time bootstrap should report Gap=0; got %d", res.GapBytes)
	}
	if res.Slot == nil || res.Slot.RestartLSN == "" {
		t.Errorf("recreated slot should have populated RestartLSN; got %+v", res.Slot)
	}
	gapEnd := res.GapEndLSN
	if gapEnd == 0 {
		t.Errorf("GapEndLSN should be populated post-recreation; got 0")
	}

	// Case 2: now drop again and re-ensure with a
	// last_confirmed below the current restart_lsn → genuine gap.
	if err := replication.DropSlot(context.Background(), repl, slot); err != nil {
		t.Fatalf("drop slot: %v", err)
	}
	priorConfirmed := pglogrepl.LSN(1) // far behind PG's actual position
	res2, err := replication.EnsureSlot(context.Background(), reg, repl, slot, priorConfirmed)
	if err != nil {
		t.Fatalf("EnsureSlot (case 2): %v", err)
	}
	if res2.Outcome != replication.SlotRecreated {
		t.Errorf("case 2 Outcome = %q, want %q", res2.Outcome, replication.SlotRecreated)
	}
	if !res2.HasGap() {
		t.Errorf("case 2 should report a gap (lastConfirmed=1, restart_lsn = current xlog pos)")
	}
	if res2.GapStartLSN != priorConfirmed {
		t.Errorf("case 2 GapStartLSN = %v, want %v", res2.GapStartLSN, priorConfirmed)
	}
	if res2.GapEndLSN <= priorConfirmed {
		t.Errorf("case 2 GapEndLSN = %v should be > priorConfirmed %v", res2.GapEndLSN, priorConfirmed)
	}
	if uint64(res2.GapEndLSN-priorConfirmed) != res2.GapBytes {
		t.Errorf("case 2 GapBytes %d != GapEndLSN(%v) - GapStartLSN(%v)",
			res2.GapBytes, res2.GapEndLSN, priorConfirmed)
	}
}
