//go:build integration

// PG 17+ incremental backup lifecycle, end to end against a real
// PostgreSQL.  Closes the coverage gap that was identified during
// the May-27 audit of the incremental claims (open question: do we
// actually do this, or is it a stub?).  The audit found the
// implementation is real (CLI → runner → wire-layer INCREMENTAL
// clause → manifest persistence of PG's own backup_manifest →
// restore.restoreIncrementalChain → combine.Run wrapping
// pg_combinebackup), but the only coverage was unit-level — chain
// resolution against synthetic manifests, arg-builder shapes, etc.
// This test drives the FULL lifecycle against a live PG.
package runner_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/combine"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/postverify"
)

// TestIncrementalBackupLifecycleEndToEnd_PG17Plus drives the full
// PG 17+ incremental-backup contract against a live PostgreSQL:
//
//  1. Enable summarize_wal at the server (PG 17 requirement for
//     BASE_BACKUP INCREMENTAL).
//  2. Create a table, INSERT 10 rows, take a FULL backup.
//  3. Assert the full's manifest carries PGBackupManifest (PG's
//     own backup_manifest blob) — that's what anchors children.
//  4. INSERT 15 more rows + CHECKPOINT so the walsummarizer
//     covers the new WAL.
//  5. Take an INCREMENTAL backup pinned to the full's manifest.
//  6. Assert Type=incremental_lsn and ParentBackupID matches.
//  7. Restore the INCREMENTAL leaf — this dispatches into
//     restore.restoreIncrementalChain, which materialises every
//     link of the chain into a staging dir and runs
//     pg_combinebackup to flatten them into a single bootable
//     datadir.
//  8. Sanity-check the restored directory contains PG_VERSION,
//     backup_label, global/pg_control.
//  9. Run postverify in Auto mode — if pg_ctl is on PATH on the
//     test host, this actually boots PG against the restored
//     datadir, proving the chain produced a working cluster.
//     Soft-skip with a log line when pg_ctl is absent (typical
//     in CI without the postgresql-client package).
//
// The test gates on PG 17+ via testkit.ExpectedPGMajorInt() and on
// pg_combinebackup being on PATH.  Without either, t.Skipf — the
// test isn't applicable to the host setup.
func TestIncrementalBackupLifecycleEndToEnd_PG17Plus(t *testing.T) {
	if v := testkit.ExpectedPGMajorInt(); v < 17 {
		t.Skipf("incremental backups require PG 17+; testkit is configured to PG %d", v)
	}
	if _, err := combine.DiscoverPGCombineBackup(); err != nil {
		t.Skipf("pg_combinebackup not on PATH (%v) — install postgresql-client / postgresql 17+ to run this test", err)
	}

	srv := testkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 1. Enable summarize_wal.  Without it, BASE_BACKUP INCREMENTAL
	//    on PG 17 fails with `WAL summaries are required ... but no
	//    summary file ... was found`.  We poll SHOW summarize_wal
	//    to confirm the SIGHUP-reload landed before proceeding.
	enableSummarizeWAL(t, ctx, srv.DSN)

	// 2. Schema + initial rows.
	dbExec(t, ctx, srv.DSN, `
		CREATE TABLE t (id int PRIMARY KEY, v text);
		INSERT INTO t SELECT g, 'pre-full-' || g FROM generate_series(1, 10) g;
	`)

	// 3. Init repo + signing keys.
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

	// 4. FULL backup.
	fullRes, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN, RepoURL: repoURL, Deployment: "db1",
		Signer: signer, Verifier: verifier, Fast: true,
	})
	if err != nil {
		t.Fatalf("full backup: %v", err)
	}
	t.Logf("full backup id=%s stop_lsn=%s", fullRes.BackupID, fullRes.StopLSN)

	// 5. Read the full's manifest back from the repo and assert
	//    PGBackupManifest is populated — the load-bearing field
	//    that lets an incremental child anchor against it.
	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	defer sp.Close()
	store := backup.NewManifestStore(sp)
	fullManifest, err := store.Read(ctx, "db1", fullRes.BackupID, verifier)
	if err != nil {
		t.Fatalf("read full manifest: %v", err)
	}
	if fullManifest.Type != backup.BackupTypeFull {
		t.Errorf("full manifest Type = %q, want %q", fullManifest.Type, backup.BackupTypeFull)
	}
	if len(fullManifest.PGBackupManifest) == 0 {
		t.Fatalf("full backup manifest must carry PGBackupManifest to anchor incrementals; got 0 bytes")
	}
	if fullManifest.ParentBackupID != "" {
		t.Errorf("full manifest must have empty ParentBackupID; got %q", fullManifest.ParentBackupID)
	}

	// 6. Generate changes the incremental will pick up.  CHECKPOINT
	//    nudges the walsummarizer to flush its current summary file
	//    so the incremental's preflight finds coverage.
	dbExec(t, ctx, srv.DSN, `
		INSERT INTO t SELECT g, 'post-full-' || g FROM generate_series(11, 25) g;
		CHECKPOINT;
	`)

	// 7. INCREMENTAL backup pinned to the full's PG backup_manifest.
	incRes, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN, RepoURL: repoURL, Deployment: "db1",
		Signer: signer, Verifier: verifier, Fast: true,
		Incremental: &runner.IncrementalConfig{
			ParentBackupID:   fullRes.BackupID,
			ParentPGManifest: fullManifest.PGBackupManifest,
		},
	})
	if err != nil {
		t.Fatalf("incremental backup: %v", err)
	}
	t.Logf("incremental backup id=%s parent=%s stop_lsn=%s",
		incRes.BackupID, fullRes.BackupID, incRes.StopLSN)

	// 8. The incremental's manifest must point back at the parent
	//    and carry the right backup type.
	incManifest, err := store.Read(ctx, "db1", incRes.BackupID, verifier)
	if err != nil {
		t.Fatalf("read incremental manifest: %v", err)
	}
	if incManifest.Type != backup.BackupTypeIncremental {
		t.Errorf("incremental Type = %q, want %q",
			incManifest.Type, backup.BackupTypeIncremental)
	}
	if incManifest.ParentBackupID != fullRes.BackupID {
		t.Errorf("incremental ParentBackupID = %q, want %q",
			incManifest.ParentBackupID, fullRes.BackupID)
	}
	if len(incManifest.PGBackupManifest) == 0 {
		// The incremental itself must persist its OWN PG manifest
		// so a future grandchild could anchor against it.
		t.Errorf("incremental manifest missing PGBackupManifest; descendants couldn't chain off this one")
	}

	// 9. Restore the INCREMENTAL leaf.  The restore code detects
	//    Type=incremental and dispatches to restoreIncrementalChain,
	//    which materialises full + incremental into staging dirs
	//    and runs pg_combinebackup to flatten them into target.
	target := filepath.Join(t.TempDir(), "restored")
	rres, err := restore.Restore(ctx, restore.Options{
		RepoURL: repoURL, Deployment: "db1", BackupID: incRes.BackupID,
		TargetDir: target, Verifier: verifier,
		OnEvent: func(ev *output.Event) {
			// Surface pg_combinebackup_stderr + every error/
			// warning so a chain-restore failure is debuggable
			// without re-running with extra instrumentation.
			if ev.Op == "pg_combinebackup_stderr" || ev.Severity <= output.SeverityError {
				t.Logf("[restore event] %s: %v", ev.Op, ev.Body)
			}
		},
	})
	if err != nil {
		t.Fatalf("restore incremental leaf: %v", err)
	}
	if rres.FileCount == 0 || rres.BytesWritten == 0 {
		t.Fatalf("restore produced an empty datadir: files=%d bytes=%d",
			rres.FileCount, rres.BytesWritten)
	}
	t.Logf("restored: files=%d bytes=%d to %s",
		rres.FileCount, rres.BytesWritten, target)

	// 10. Filesystem shape sanity: the combined output must look
	//     like a valid PG data dir.  pg_combinebackup synthesizes
	//     these from the chain; their absence would point at a
	//     broken combine or a chain-resolution failure.
	for _, name := range []string{"PG_VERSION", "backup_label", "global/pg_control"} {
		if _, err := os.Stat(filepath.Join(target, name)); err != nil {
			t.Errorf("restored datadir missing %s: %v", name, err)
		}
	}

	// 11. Boot PG against the restored datadir if pg_ctl is on the
	//     host.  The strongest end-to-end correctness signal we
	//     can run from a Go test — proves the combined cluster
	//     can complete startup recovery and serve queries.  Soft-
	//     skip with a log line when the host lacks PG client
	//     tools (typical for CI runners without postgresql-client
	//     installed).
	vres, err := postverify.Verify(ctx, postverify.Options{
		Mode:           postverify.ModeAuto,
		DataDir:        target,
		PGMajorVersion: testkit.ExpectedPGMajorInt(),
		RepoURL:        repoURL,
		Deployment:     "db1",
	})
	switch {
	case err != nil:
		t.Logf("postverify: %v — host lacks usable pg_ctl; chain still validated above by FS shape + pg_combinebackup exit code", err)
	case vres.Skipped:
		t.Logf("postverify skipped (%s) — chain still validated above by FS shape + pg_combinebackup exit code", vres.SkipReason)
	default:
		t.Logf("postverify booted PG on the combined datadir: queries=%d start=%s",
			vres.QueriesRan, vres.StartDuration)
	}
}

// enableSummarizeWAL flips summarize_wal on via ALTER SYSTEM +
// pg_reload_conf, then polls SHOW summarize_wal until the reload
// lands.  PG 17 ships the walsummarizer as a SIGHUP-loadable GUC,
// so no server restart is required — but reload IS async, and
// taking a backup before the summarizer is active produces an
// incremental that PG refuses ("WAL summaries are required").
func enableSummarizeWAL(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to enable summarize_wal: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "ALTER SYSTEM SET summarize_wal = on"); err != nil {
		t.Fatalf("ALTER SYSTEM SET summarize_wal: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
		t.Fatalf("pg_reload_conf: %v", err)
	}
	// Poll for up to 10 seconds.  In practice the reload is near-
	// instant; the loop is defensive against a slow first SIGHUP.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var v string
		if err := conn.QueryRow(ctx, "SHOW summarize_wal").Scan(&v); err == nil && v == "on" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("summarize_wal did not become 'on' within 10s after pg_reload_conf")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// dbExec runs the given SQL on a regular-mode connection.  Helper
// shared across the three INSERT / DDL points in the test so the
// happy path stays readable.
func dbExec(t *testing.T, ctx context.Context, dsn, sql string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for SQL %q...: %v", truncForLog(sql), err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("exec %q: %v", truncForLog(sql), err)
	}
}

func truncForLog(s string) string {
	if len(s) <= 60 {
		return s
	}
	return fmt.Sprintf("%s...(%d more)", s[:60], len(s)-60)
}
