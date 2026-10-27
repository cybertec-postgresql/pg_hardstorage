//go:build integration

package runner_test

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/leakcheck"
)

// TestLeak_TakeRestoreRepeated runs backup -> chunk-existence check ->
// restore against a real PostgreSQL many times in ONE process and proves
// the process returns to its fd / goroutine / mmap baseline and that the
// live heap plateaus. PGHS_LEAK_CYCLES overrides the cycle count.
func TestLeak_TakeRestoreRepeated(t *testing.T) {
	srv := testkit.StartPostgres(t)
	cycles := 12
	if os.Getenv("PGHS_SCALE") == "1" {
		cycles = 60
	}
	if v := os.Getenv("PGHS_LEAK_CYCLES"); v != "" {
		cycles, _ = strconv.Atoi(v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	work := t.TempDir()

	cycle := func(i int) {
		res, err := runner.Take(ctx, runner.TakeOptions{
			IncludeWAL: true, PGConnString: srv.DSN, RepoURL: repoURL, Deployment: "db1",
			Signer: signer, Verifier: verifier, Fast: true,
		})
		if err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
		_, sp, err := repo.Open(ctx, repoURL)
		if err != nil {
			t.Fatal(err)
		}
		ms := backup.NewManifestStore(sp)
		m, err := ms.ReadStreaming(ctx, "db1", res.BackupID, verifier)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.EachFile(ctx, func(*backup.FileEntry) error { return nil }); err != nil {
			t.Fatal(err)
		}
		_ = sp.Close()
		target := filepath.Join(work, "r"+strconv.Itoa(i))
		if _, err := restore.Restore(ctx, restore.Options{
			RepoURL: repoURL, Deployment: "db1", BackupID: res.BackupID, TargetDir: target, Verifier: verifier,
			PGVerifyBackup: restore.VerifySkip,
		}); err != nil {
			t.Fatalf("restore %d: %v", i, err)
		}
		_ = os.RemoveAll(target)
	}
	for i := 0; i < 2; i++ { // warm-up: lazy singletons, TLS/http pools, metrics
		cycle(-i - 1)
	}
	base := leakcheck.Take()
	leakcheck.Plateau(t, cycles, cycles/8, 32<<20, cycle)
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{Goroutines: 3, FDs: 2})
}

// TestLeak_TakeCancelledMidway aborts a backup at assorted points (connect,
// base-backup stream, chunk upload, manifest commit) many times in one
// process. Every abort path must release its goroutines, fds, spool temp
// files and pg connections.
func TestLeak_TakeCancelledMidway(t *testing.T) {
	srv := testkit.StartPostgres(t)
	cycles := 14
	if os.Getenv("PGHS_SCALE") == "1" {
		cycles = 80
	}
	if v := os.Getenv("PGHS_LEAK_CYCLES"); v != "" {
		cycles, _ = strconv.Atoi(v)
	}
	repoURL := "file://" + t.TempDir()
	bg := context.Background()
	if _, err := repo.Init(bg, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	cycle := func(i int) {
		d := time.Duration(80+(i*397)%2400) * time.Millisecond
		ctx, cancel := context.WithTimeout(bg, d)
		defer cancel()
		start := time.Now()
		_, err := runner.Take(ctx, runner.TakeOptions{
			IncludeWAL: true, PGConnString: srv.DSN, RepoURL: repoURL, Deployment: "db1",
			Signer: signer, Verifier: verifier, Fast: true,
		})
		msg := "<nil>"
		if err != nil {
			msg = err.Error()
			if len(msg) > 120 {
				msg = msg[:120]
			}
		}
		t.Logf("cycle %d: budget %v, ran %v: %s", i, d, time.Since(start).Round(time.Millisecond), msg)
	}
	for i := 0; i < 2; i++ {
		cycle(i)
	}
	time.Sleep(2 * time.Second)
	before := len(leakcheck.DirEntries(tmp))
	base := leakcheck.Take()
	leakcheck.Plateau(t, cycles, cycles/8, 32<<20, cycle)
	time.Sleep(3 * time.Second) // let server-side teardown and any stragglers finish
	if after := leakcheck.DirEntries(tmp); len(after) > before {
		t.Errorf("temp files left behind by aborted backups: %v", after)
	}
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{Goroutines: 3, FDs: 2})
}
