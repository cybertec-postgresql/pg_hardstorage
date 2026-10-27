package restore_test

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/leakcheck"
)

// TestLeak_RestoreRepeated restores the same backup many times in one
// process (successful, cancelled, wrong-key and resume variants) and
// checks fds, goroutines, mappings and heap return to baseline.
func TestLeak_RestoreRepeated(t *testing.T) {
	fx := newFixture(t)
	_, wrongPub, _ := backup.GenerateKeypair(rand.Reader)
	wrongV, _ := backup.LoadVerifier(wrongPub)
	n := 120
	if os.Getenv("PGHS_SCALE") == "1" {
		n = 2000
	}
	work := t.TempDir()
	opts := func(i int) restore.Options {
		return restore.Options{
			RepoURL: fx.repoURL, Deployment: "db1", BackupID: "db1.full.20260428T130000Z.0001",
			TargetDir: filepath.Join(work, "r"+strconv.Itoa(i)), Verifier: fx.verifier,
			PGVerifyBackup: restore.VerifySkip,
		}
	}
	// warm-up so one-time lazies (metrics, http pools) are in the baseline
	for i := 0; i < 3; i++ {
		o := opts(-i - 1)
		if _, err := restore.Restore(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		_ = os.RemoveAll(o.TargetDir)
	}
	base := leakcheck.Take()
	leakcheck.Plateau(t, n, n/16, 8<<20, func(i int) {
		o := opts(i)
		switch i % 4 {
		case 0, 1:
			if _, err := restore.Restore(context.Background(), o); err != nil {
				t.Fatal(err)
			}
		case 2:
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, _ = restore.Restore(ctx, o)
		case 3:
			o.Verifier = wrongV
			if _, err := restore.Restore(context.Background(), o); err == nil {
				t.Fatal("wrong key accepted")
			}
		}
		_ = os.RemoveAll(o.TargetDir)
	})
	_ = time.Second
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{Goroutines: 2})
}
