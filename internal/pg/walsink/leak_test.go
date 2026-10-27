package walsink_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/leakcheck"
)

// TestLeak_SinkLifecycle creates, feeds and closes (cleanly, with a fault
// injected, with a cancelled Close context) many Sinks in one process and
// proves the processor goroutine, the per-Sink segment buffer pool and the
// chunk worker goroutines are all released.
func TestLeak_SinkLifecycle(t *testing.T) {
	const seg = 1 << 20
	_, repoPath := t.TempDir(), t.TempDir()
	sp, cas := openFSRepo(t, "file://"+repoPath)
	defer sp.Close()
	n := 60
	if os.Getenv("PGHS_SCALE") == "1" {
		n = 600
	}
	rng := rand.New(rand.NewSource(1))
	data := make([]byte, 3*seg)
	rng.Read(data)

	run := func(i int) {
		opts := walsink.Options{Deployment: "db1", Timeline: 1, SystemIdentifier: "7388123456789", SegmentSize: seg}
		var faultErr error
		if i%3 == 1 {
			faultErr = errors.New("injected crash")
			opts.FaultHook = func(_ context.Context, cp string) error {
				if cp == walsink.HookBeforeManifestCommit {
					return faultErr
				}
				return nil
			}
		}
		s, err := walsink.New(cas, sp, opts)
		if err != nil {
			t.Fatal(err)
		}
		base := uint64(i%50) * 3 * seg
		_ = s.OnRecord(context.Background(), replication.XLogRecord{WALStart: pglogrepl.LSN(base), Data: data})
		switch i % 3 {
		case 0:
			if err := s.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		case 1:
			_ = s.Close(context.Background()) // fault surfaces here
		case 2:
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_ = s.Close(ctx)
		}
		_ = s.Close(context.Background())
	}
	for i := 0; i < 3; i++ {
		run(i)
	}
	base := leakcheck.Take()
	leakcheck.Plateau(t, n, n/16, 16<<20, run)
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{})
}

// archive_command shim: many PushSegmentFile calls, including rejects.
func TestLeak_PushSegmentFileRepeated(t *testing.T) {
	sp, cas := openFSRepo(t, "file://"+t.TempDir())
	defer sp.Close()
	src := t.TempDir()
	body := make([]byte, walsink.SegmentSize)
	rand.New(rand.NewSource(2)).Read(body)
	n := 80
	if os.Getenv("PGHS_SCALE") == "1" {
		n = 800
	}
	var paths []string
	for i := 0; i < 4; i++ {
		p := filepath.Join(src, fmt.Sprintf("0000000100000000%08X", i+1))
		if err := os.WriteFile(p, body, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	short := filepath.Join(src, "000000010000000000000009")
	_ = os.WriteFile(short, body[:1000], 0o600)
	step := func(i int) {
		var err error
		if i%5 == 4 {
			_, err = walsink.PushSegmentFile(context.Background(), cas, sp, short, walsink.PushOptions{Deployment: "db1", SystemIdentifier: "7000000000000000001"})
			if err == nil {
				t.Fatal("short segment accepted")
			}
			return
		}
		if _, err = walsink.PushSegmentFile(context.Background(), cas, sp, paths[i%4], walsink.PushOptions{Deployment: "db1", SystemIdentifier: "7000000000000000001"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		step(i)
	}
	base := leakcheck.Take()
	leakcheck.Plateau(t, n, n/16, 16<<20, step)
	_ = time.Second
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{})
}
