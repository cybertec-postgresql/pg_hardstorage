// Build-tagged integration test: streams real WAL from a containerised
// PG into walsink and asserts at least one segment manifest commits.
//
//go:build integration

package walsink_test

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestIntegration_Walsink_CommitsSegmentFromRealWAL drives a Sink off
// the live replication stream of a freshly-started PG and asserts at
// least one 16 MiB segment commits — proving the WAL pipeline is
// end-to-end functional. Generates ~32 MiB of WAL via bulk INSERT to
// guarantee a segment fills within the test window.
func TestIntegration_Walsink_CommitsSegmentFromRealWAL(t *testing.T) {
	srv := testkit.StartPostgres(t)

	// Stand up a fresh repo for the test.
	repoRoot := t.TempDir()
	repoURL := "file://" + repoRoot
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo.Init: %v", err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: mustParseURL(t, repoURL)}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	cas := repo.NewCAS(sp)

	// Capture system_identifier + timeline on the replication connection.
	idConn, err := pg.Connect(context.Background(), srv.DSN, pg.ModeReplication)
	if err != nil {
		t.Fatalf("connect (replication): %v", err)
	}
	identity, err := pg.IdentifySystem(context.Background(), idConn)
	if err != nil {
		_ = idConn.Close(context.Background())
		t.Fatalf("IDENTIFY_SYSTEM: %v", err)
	}
	_ = idConn.Close(context.Background())

	// Force a WAL switch + CHECKPOINT BEFORE creating the slot so the
	// next write position lands at a fresh 16 MiB segment boundary.
	// walsink rejects streams that begin mid-segment ("out-of-order
	// WAL: segment N expected offset 0 got M") because it can only
	// finalize whole 16 MiB units; an arbitrary restart_lsn from a
	// busy cluster won't be segment-aligned.  Switching first means
	// the RESERVE_WAL slot reserves at the new segment's start.
	prepConn, err := pg.Connect(context.Background(), srv.DSN, pg.ModeRegular)
	if err != nil {
		t.Fatalf("connect for pre-slot WAL switch: %v", err)
	}
	_ = prepConn.PgConn().ExecParams(context.Background(), "CHECKPOINT", nil, nil, nil, nil).Read()
	_ = prepConn.PgConn().ExecParams(context.Background(), "SELECT pg_switch_wal()", nil, nil, nil, nil).Read()
	_ = prepConn.Close(context.Background())

	// Create the replication slot.
	slotConn, err := pg.Connect(context.Background(), srv.DSN, pg.ModeReplication)
	if err != nil {
		t.Fatalf("connect for slot create: %v", err)
	}
	const slot = "hsctl_walsink_test"
	// RESERVE_WAL so the slot's restart_lsn is populated at
	// creation time.  See the matching comment in
	// internal/pg/replication/integration_test.go's
	// TestIntegration_StreamReceivesXLogData for why plain
	// CreatePhysicalSlot makes Stream ask for segment 0 of a
	// fresh cluster (which doesn't exist).
	if err := replication.CreatePhysicalSlotReserveWAL(context.Background(), slotConn, slot); err != nil {
		_ = slotConn.Close(context.Background())
		t.Fatalf("CreatePhysicalSlotReserveWAL: %v", err)
	}
	_ = slotConn.Close(context.Background())
	t.Cleanup(func() {
		c, err := pg.Connect(context.Background(), srv.DSN, pg.ModeReplication)
		if err != nil {
			return
		}
		_ = replication.DropSlot(context.Background(), c, slot)
		_ = c.Close(context.Background())
	})

	// Read the slot's restart_lsn (populated by RESERVE_WAL above) so
	// we can pass it as StartLSN to Stream.  Without this, Stream
	// defaults to LSN 0/0 → START_REPLICATION asks PG for segment 0
	// of timeline 1, which initdb advances past before the cluster
	// is ready, so PG fails the request with "requested WAL segment
	// 000000010000000000000000 has already been removed" and the
	// sink never sees a single record.  Same pattern continuity.go
	// uses on slot recreation.
	regConn, err := pg.Connect(context.Background(), srv.DSN, pg.ModeRegular)
	if err != nil {
		t.Fatalf("connect for slot lookup: %v", err)
	}
	slotInfo, err := replication.GetSlot(context.Background(), regConn, slot)
	_ = regConn.Close(context.Background())
	if err != nil {
		t.Fatalf("GetSlot after RESERVE_WAL: %v", err)
	}
	if slotInfo.RestartLSN == "" {
		t.Fatalf("restart_lsn empty after RESERVE_WAL; cannot pick a safe StartLSN")
	}
	startLSN, err := pglogrepl.ParseLSN(slotInfo.RestartLSN)
	if err != nil {
		t.Fatalf("parse restart_lsn %q: %v", slotInfo.RestartLSN, err)
	}
	// Align DOWN to the segment containing restart_lsn — the segment
	// is retained by the slot (RESERVE_WAL pinned it), and walsink
	// requires the first record to land at the segment boundary.
	// Same calculation as resolveStartLSN's "fresh-slot-restart-lsn"
	// branch in internal/cli/wal.go.
	startLSN = pglogrepl.LSN(uint64(startLSN) &^ uint64(walsink.SegmentSize-1))

	// Build the Sink against the captured identity.
	sink, err := walsink.New(cas, sp, walsink.Options{
		Deployment:       "db1",
		Timeline:         uint32(identity.Timeline),
		SystemIdentifier: identity.SystemID,
	})
	if err != nil {
		t.Fatalf("walsink.New: %v", err)
	}

	// Generate WAL in a goroutine while we stream. ~32 MiB worth of
	// rows guarantees at least one segment fills.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		c, err := pg.Connect(ctx, srv.DSN, pg.ModeRegular)
		if err != nil {
			return
		}
		defer c.Close(ctx)
		// Three switches force PG to advance segments quickly.
		_ = c.PgConn().ExecParams(ctx, "SELECT pg_switch_wal()", nil, nil, nil, nil).Read()
		_ = c.PgConn().ExecParams(ctx, "CREATE TABLE walsink_t (i int, payload text)", nil, nil, nil, nil).Read()
		// Insert ~32 MiB: 16 KiB per row × 2048 rows.
		_ = c.PgConn().ExecParams(ctx,
			"INSERT INTO walsink_t SELECT g, repeat('x', 16384) FROM generate_series(1, 2048) g",
			nil, nil, nil, nil).Read()
		_ = c.PgConn().ExecParams(ctx, "CHECKPOINT", nil, nil, nil, nil).Read()
		_ = c.PgConn().ExecParams(ctx, "SELECT pg_switch_wal()", nil, nil, nil, nil).Read()
		// Wait so segments have time to commit through the sink, then
		// cancel to terminate Stream cleanly.
		time.Sleep(8 * time.Second)
		cancel()
	}()

	// Stream into the Sink.
	streamConn, err := pg.Connect(context.Background(), srv.DSN, pg.ModeReplication)
	if err != nil {
		t.Fatalf("connect for stream: %v", err)
	}
	streamErr := replication.Stream(ctx, streamConn, replication.StreamOptions{
		Slot:                 slot,
		StartLSN:             startLSN,
		Timeline:             uint32(identity.Timeline),
		StatusUpdateInterval: 250 * time.Millisecond,
	}, sink)
	if !errors.Is(streamErr, context.Canceled) {
		t.Errorf("expected context.Canceled; got %v", streamErr)
	}

	// Drain the async sink: the processor may still be committing
	// segments the receive side handed off before Stream stopped.
	// Background ctx (not the cancelled stream ctx) so it fully drains.
	if err := sink.Close(context.Background()); err != nil {
		t.Fatalf("sink.Close: %v", err)
	}

	// Assert: SyncedLSN advanced past the start, indicating at least
	// one segment committed.
	if got := uint64(sink.SyncedLSN()); got < walsink.SegmentSize {
		t.Errorf("SyncedLSN = %x; want at least %x (one segment committed)",
			got, walsink.SegmentSize)
	}

	// Assert: at least one manifest exists under wal/db1/<TLI>/.
	prefix := walsink.SegmentPath("db1", uint32(identity.Timeline), "")
	prefix = prefix[:len(prefix)-len(".json")] // strip the trailing ".json" added by SegmentPath for empty name
	count := 0
	for info, listErr := range sp.List(context.Background(), "wal/db1/") {
		if listErr != nil {
			t.Fatalf("list: %v", listErr)
		}
		t.Logf("found WAL manifest: %s (%d bytes)", info.Key, info.Size)
		count++
	}
	if count == 0 {
		t.Errorf("expected at least one segment manifest in repo; found 0 (prefix probed: %s)", prefix)
	}
}

func mustParseURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
