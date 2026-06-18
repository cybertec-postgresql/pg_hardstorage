//go:build integration

package replication_test

import (
	"context"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
)

// TestReplicationSlotManagement exercises the physical-slot lifecycle
// against a real PG: create (reserving WAL) → inspect via
// pg_replication_slots → drop.
func TestReplicationSlotManagement(t *testing.T) {
	srv := testkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	conn, err := pg.Connect(ctx, srv.DSN, pg.ModeReplication)
	if err != nil {
		t.Fatalf("replication connect: %v", err)
	}
	defer conn.Close(ctx)

	identity, err := pg.IdentifySystem(ctx, conn)
	if err != nil {
		t.Fatalf("IDENTIFY_SYSTEM: %v", err)
	}
	t.Logf("cluster: system=%s timeline=%d xlogpos=%s", identity.SystemID, identity.Timeline, identity.XLogPos)

	slotName := "repl_test_slot"
	if err := replication.CreatePhysicalSlotReserveWAL(ctx, conn, slotName); err != nil {
		t.Fatalf("CreatePhysicalSlotReserveWAL: %v", err)
	}
	t.Logf("slot created: %s", slotName)

	regularConn, err := pg.Connect(ctx, srv.DSN, pg.ModeRegular)
	if err != nil {
		t.Fatalf("regular connect: %v", err)
	}
	defer regularConn.Close(ctx)

	res := regularConn.PgConn().ExecParams(ctx,
		"SELECT slot_type, restart_lsn::text FROM pg_replication_slots WHERE slot_name = $1",
		[][]byte{[]byte(slotName)}, nil, nil, nil).Read()
	if res.Err != nil {
		t.Fatalf("query pg_replication_slots: %v", res.Err)
	}
	if len(res.Rows) == 0 {
		t.Fatalf("slot %q not found in pg_replication_slots after create", slotName)
	}
	slotType := string(res.Rows[0][0])
	restartLSN := string(res.Rows[0][1])
	if slotType != "physical" {
		t.Errorf("expected physical slot, got %q", slotType)
	}
	t.Logf("slot exists in pg_replication_slots: type=%s restart_lsn=%s", slotType, restartLSN)

	if err := replication.DropSlot(ctx, conn, slotName); err != nil {
		t.Errorf("DropSlot: %v", err)
	}

	// Confirm the drop took effect.
	res = regularConn.PgConn().ExecParams(ctx,
		"SELECT count(*) FROM pg_replication_slots WHERE slot_name = $1",
		[][]byte{[]byte(slotName)}, nil, nil, nil).Read()
	if res.Err != nil {
		t.Fatalf("re-query pg_replication_slots: %v", res.Err)
	}
	if len(res.Rows) > 0 && string(res.Rows[0][0]) != "0" {
		t.Errorf("slot %q still present after DropSlot (count=%s)", slotName, res.Rows[0][0])
	}

	t.Log("replication slot lifecycle: create → inspect → drop OK")
}
