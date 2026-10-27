//go:build integration

package replication_test

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/leakcheck"
)

// TestLeak_WALStreamReconnectLoop is the streamer's reconnect loop in
// miniature: against a real PG, repeatedly connect, START_REPLICATION into
// a real walsink.Sink (chunk -> CAS -> manifest), switch WAL, cancel, close
// the sink and the connection. fds, goroutines, and heap must return to
// baseline; the slot must never leak a pg-side walsender.
func TestLeak_WALStreamReconnectLoop(t *testing.T) {
	srv := testkit.StartPostgres(t)
	cycles := 15
	if os.Getenv("PGHS_SCALE") == "1" {
		cycles = 120
	}
	if v := os.Getenv("PGHS_LEAK_CYCLES"); v != "" {
		cycles, _ = strconv.Atoi(v)
	}
	bg := context.Background()
	repl := connectReplication(t, srv.DSN)
	const slot = "hs_leak_slot"
	if err := replication.CreatePhysicalSlotReserveWAL(bg, repl, slot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r2 := connectReplication(t, srv.DSN)
		_ = replication.DropSlot(bg, r2, slot)
	})
	reg := connectRegular(t, srv.DSN)
	sysID := func() string {
		rr := reg.PgConn().ExecParams(bg, "SELECT system_identifier::text FROM pg_control_system()", nil, nil, nil, nil).Read()
		if rr.Err != nil || len(rr.Rows) != 1 {
			t.Fatalf("sysid: %v", rr.Err)
		}
		return string(rr.Rows[0][0])
	}()

	sp := &fs.Plugin{}
	if err := sp.Open(bg, storage.StorageConfig{URL: mustURL(t.TempDir())}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	cas := casdefault.New(sp)

	cycle := func(i int) {
		info, err := replication.GetSlot(bg, reg, slot)
		if err != nil {
			t.Fatal(err)
		}
		start, err := pglogrepl.ParseLSN(info.RestartLSN)
		if err != nil {
			t.Fatal(err)
		}
		// Segment-align: the sink expects to start at a segment boundary.
		start -= start % pglogrepl.LSN(walsink.SegmentSize)
		sink, err := walsink.New(cas, sp, walsink.Options{
			Deployment: "db1", Timeline: 1, SystemIdentifier: sysID,
			ExpectedFirstLSN: start,
		})
		if err != nil {
			t.Fatal(err)
		}
		conn, err := pg.Connect(bg, srv.DSN, pg.ModeReplication)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(bg, 4*time.Second)
		go func() {
			time.Sleep(700 * time.Millisecond)
			_ = reg.PgConn().ExecParams(bg, "CREATE TABLE IF NOT EXISTS hs_leak (i int)", nil, nil, nil, nil).Read()
			_ = reg.PgConn().ExecParams(bg, "INSERT INTO hs_leak SELECT generate_series(1,2000)", nil, nil, nil, nil).Read()
			_ = reg.PgConn().ExecParams(bg, "SELECT pg_switch_wal()", nil, nil, nil, nil).Read()
			time.Sleep(700 * time.Millisecond)
			cancel()
		}()
		_ = replication.Stream(ctx, conn, replication.StreamOptions{Slot: slot, StartLSN: start, StatusUpdateInterval: 200 * time.Millisecond}, sink)
		cancel()
		_ = sink.Close(bg)
		_ = conn.Close(bg)
	}
	for i := 0; i < 2; i++ {
		cycle(-i - 1)
	}
	base := leakcheck.Take()
	leakcheck.Plateau(t, cycles, cycles/8, 32<<20, cycle)
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{Goroutines: 3, FDs: 2})
	segs := 0
	for _, err := range sp.List(bg, "wal/") {
		if err != nil {
			t.Fatal(err)
		}
		segs++
	}
	t.Logf("repo holds %d wal/ objects", segs)
	if segs == 0 {
		t.Fatal("no WAL reached the repo: the loop did not exercise the sink")
	}
}

func mustURL(path string) *url.URL { return &url.URL{Scheme: "file", Path: path} }
