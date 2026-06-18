//go:build integration

// Regression: a control-plane PITR restore (the operator POSTs a
// restore with a to_lsn / to / to_name target and an agent claims it)
// must succeed end to end. The agent's RestoreExecutor builds the
// Recovery from the job args; that Recovery has Enable=true but no
// RestoreCommand of its own, and restore.WriteRecoveryFiles →
// validateRecovery hard-requires a non-empty RestoreCommand. Without
// the executor wiring its own wal-fetch shim into RestoreCommand,
// every control-plane PITR restore failed with restore.recovery_write
// ("RestoreCommand is required when Enable=true"). This test drives the
// real executor against a real backup and asserts the recovery files
// land.

package agent_test

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/agent"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func TestAgentExecutor_ControlPlanePITR_ArmsRecovery(t *testing.T) {
	srv := testkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo.Init: %v", err)
	}

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	res, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN, RepoURL: repoURL, Deployment: "db1",
		Signer: signer, Verifier: verifier, Fast: true,
	})
	if err != nil {
		t.Fatalf("Take: %v", err)
	}

	exec := agent.NewRestoreExecutor(
		map[string]config.DeploymentConfig{"db1": {Repo: repoURL}},
		verifier, "",
	)

	target := filepath.Join(t.TempDir(), "cp_pitr_restored")

	// A reachable PITR target: the backup's own stop LSN is always
	// reachable inclusively, so CheckTargetReachable passes and we
	// exercise the recovery-file write that the bug broke.
	toLSN := res.StopLSN
	if toLSN == "" {
		t.Fatal("backup result has no StopLSN to target")
	}

	out, err := exec.Execute(ctx, &agent.ControlPlaneJob{
		ID:         "job-cp-pitr-1",
		Kind:       "restore",
		Deployment: "db1",
		RepoURL:    repoURL,
		Args: map[string]any{
			"backup_id":  res.BackupID,
			"target_dir": target,
			"to_lsn":     toLSN,
			"to_action":  "promote",
		},
	}, func(map[string]any) {})
	if err != nil {
		t.Fatalf("control-plane PITR restore failed (regression: empty RestoreCommand?): %v", err)
	}

	if out["recovery_configured"] != true {
		t.Errorf("result should report recovery_configured=true; got %v", out["recovery_configured"])
	}

	// recovery.signal must be present (PITR, not standby).
	if _, err := os.Stat(filepath.Join(target, "recovery.signal")); err != nil {
		t.Errorf("recovery.signal missing after PITR restore: %v", err)
	}

	// postgresql.auto.conf must carry a non-empty restore_command (the
	// agent's wal-fetch shim) plus the target LSN GUC.
	conf, err := os.ReadFile(filepath.Join(target, "postgresql.auto.conf"))
	if err != nil {
		t.Fatalf("read postgresql.auto.conf: %v", err)
	}
	confStr := string(conf)
	for _, want := range []string{
		"restore_command = '",
		"wal fetch",
		"recovery_target_lsn = '" + toLSN + "'",
		"recovery_target_action = 'promote'",
	} {
		if !strings.Contains(confStr, want) {
			t.Errorf("postgresql.auto.conf missing %q\n---\n%s", want, confStr)
		}
	}
}

// TestAgentExecutor_ControlPlaneLatestWithTimeTarget_ResolvesEarlierSeed
// pins the time-aware seed resolution for a control-plane restore:
// POST {"backup_id":"latest","to":"<between A and B>"} must resolve to
// the EARLIER backup A, not the unconstrained latest B. PG replays WAL
// forward from the seed checkpoint, so a seed taken after the target
// can never reach it. The executor used to call ResolveLatest
// unconditionally (ignoring `to`), so it picked B and the restored
// cluster would fail to start.
func TestAgentExecutor_ControlPlaneLatestWithTimeTarget_ResolvesEarlierSeed(t *testing.T) {
	srv := testkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo.Init: %v", err)
	}

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	take := func() *runner.Result {
		t.Helper()
		res, err := runner.Take(ctx, runner.TakeOptions{
			PGConnString: srv.DSN, RepoURL: repoURL, Deployment: "db1",
			Signer: signer, Verifier: verifier, Fast: true,
		})
		if err != nil {
			t.Fatalf("Take: %v", err)
		}
		return res
	}

	bkpA := take()
	// Ensure a clear (≥2s) stop_time gap so an RFC3339 (second-
	// precision) target can land strictly between A and B.
	time.Sleep(2500 * time.Millisecond)
	bkpB := take()

	if !bkpB.StoppedAt.After(bkpA.StoppedAt) {
		t.Fatalf("expected B (%s) to stop after A (%s)", bkpB.StoppedAt, bkpA.StoppedAt)
	}

	// Target: 1s after A stopped — at or before this point only A is a
	// valid seed; B stopped ~2.5s later and is excluded.
	target := bkpA.StoppedAt.Add(1 * time.Second).UTC().Format(time.RFC3339)

	exec := agent.NewRestoreExecutor(
		map[string]config.DeploymentConfig{"db1": {Repo: repoURL}},
		verifier, "",
	)
	out, err := exec.Execute(ctx, &agent.ControlPlaneJob{
		ID:         "job-latest-time-1",
		Kind:       "restore",
		Deployment: "db1",
		RepoURL:    repoURL,
		Args: map[string]any{
			"backup_id":  "latest",
			"target_dir": filepath.Join(t.TempDir(), "latest_time_restored"),
			"to":         target,
		},
	}, func(map[string]any) {})
	if err != nil {
		t.Fatalf("control-plane latest+time restore failed: %v", err)
	}

	if got := out["backup_id"]; got != bkpA.BackupID {
		t.Errorf("resolved seed = %v; want the EARLIER backup %s (time-aware "+
			"resolution must not pick the unconstrained latest %s)",
			got, bkpA.BackupID, bkpB.BackupID)
	}
}
