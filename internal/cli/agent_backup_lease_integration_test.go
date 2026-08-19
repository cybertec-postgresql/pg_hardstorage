//go:build integration

package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestBuildBackupTask_HonoursAllowUnenforceableLease proves the in-process
// agent schedule (buildBackupTask — the path the sidecar chart's
// `pg_hardstorage agent` uses) honours allow_unenforceable_lease. #47 wired
// this into the control-plane executor (internal/agent/executor.go) only; a
// chart-deployed agent's scheduled backups go through THIS path and would
// still fail on a lease-incapable backend (Ceph S3, some MinIO) without it.
//
// fs supports atomic conditional put, so a held lease is genuinely
// enforceable and a second acquirer is refused with ErrBackupInProgress —
// which lets us prove the behaviour without a Ceph backend: without the flag
// the scheduled backup must fail on the held lease; with it, SkipLease
// bypasses the lease and the backup runs.
func TestBuildBackupTask_HonoursAllowUnenforceableLease(t *testing.T) {
	srv := testkit.StartPostgres(t)
	ctx := context.Background()
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	lease, err := backup.AcquireBackupLease(ctx, sp, "db1", backup.LeaseOptions{Owner: "test-holder"})
	if err != nil {
		t.Fatalf("pre-hold lease: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release(context.Background()) })

	run := func(allow bool, timeout time.Duration) error {
		dep := config.DeploymentConfig{
			PGConnection:            srv.DSN,
			Repo:                    repoURL,
			Schedule:                config.DeploymentSchedule{Backup: config.ScheduleSpec{Every: "24h"}},
			AllowUnenforceableLease: allow,
		}
		task, err := buildBackupTask("db1", dep, config.KMSConfig{}, signer, verifier)
		if err != nil {
			t.Fatalf("buildBackupTask(allow=%v): %v", allow, err)
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return task.Run(cctx)
	}

	// Without the flag: the held lease blocks the in-process scheduled backup.
	if err := run(false, 90*time.Second); err == nil {
		t.Fatal("scheduled backup ran despite a held lease and allow_unenforceable_lease=false — " +
			"the lease is not being enforced on the in-process path")
	} else if !errors.Is(err, backup.ErrBackupInProgress) {
		t.Fatalf("expected ErrBackupInProgress from the held lease, got: %v", err)
	}

	// With the flag: SkipLease bypasses the held lease and the backup runs.
	if err := run(true, 180*time.Second); err != nil {
		t.Fatalf("allow_unenforceable_lease=true did not bypass the held lease on the in-process "+
			"schedule path (the #47 follow-up wiring): %v", err)
	}
}
