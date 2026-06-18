//go:build integration

package logical_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/logicalreceiver"
	pgtestkit "github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
)

// TestLag_SlotMissing — when the named slot doesn't exist in
// pg_replication_slots, Lag returns ErrSlotNotFound (so callers can
// distinguish "slot was dropped" from "PG is unreachable").
func TestLag_SlotMissing(t *testing.T) {
	pgInst := pgtestkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := logical.Lag(ctx, pgInst.DSN, "slot-that-does-not-exist")
	if !errors.Is(err, logical.ErrSlotNotFound) {
		t.Errorf("Lag on missing slot: got %v, want ErrSlotNotFound", err)
	}
}

// TestLag_FreshSlotZeroBehind — a brand-new slot with no consumer
// activity has empty confirmed_flush_lsn. Lag should populate the
// other fields (slot name, plugin, restart_lsn from creation) and
// leave BehindBytes at 0 (no comparison possible without a flush).
func TestLag_FreshSlotZeroBehind(t *testing.T) {
	pgInst := pgtestkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create a logical slot via the replication-mode connection
	// helper. The slot is persistent so subsequent Lag calls find it.
	repConn, err := pg.Connect(ctx, pgInst.DSN, pg.ModeReplication)
	if err != nil {
		t.Fatalf("connect replication: %v", err)
	}
	const slot = "logical_lag_test_slot"
	if err := logicalreceiver.CreateLogicalSlot(ctx, repConn, slot, ""); err != nil {
		repConn.Close(ctx)
		t.Fatalf("create slot: %v", err)
	}
	repConn.Close(ctx)
	t.Cleanup(func() {
		c, err := pg.Connect(context.Background(), pgInst.DSN, pg.ModeReplication)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_ = logicalreceiver.DropLogicalSlot(context.Background(), c, slot)
	})

	lag, err := logical.Lag(ctx, pgInst.DSN, slot)
	if err != nil {
		t.Fatalf("Lag: %v", err)
	}
	if lag.SlotName != slot {
		t.Errorf("SlotName = %q, want %q", lag.SlotName, slot)
	}
	if lag.Plugin != "pgoutput" {
		t.Errorf("Plugin = %q, want pgoutput", lag.Plugin)
	}
	if lag.Active {
		t.Error("Active should be false on fresh slot (no consumer attached)")
	}
	if lag.CurrentWALLSN == "" {
		t.Error("CurrentWALLSN should be populated from pg_current_wal_lsn()")
	}
	// confirmed_flush_lsn is "" until first flush; BehindBytes
	// stays 0 in that case (we can't compare to nothing).
	if lag.ConfirmedFlushLSN == "" && lag.BehindBytes != 0 {
		t.Errorf("BehindBytes = %d, want 0 when confirmed_flush_lsn is empty", lag.BehindBytes)
	}
}
