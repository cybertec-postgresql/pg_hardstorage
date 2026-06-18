//go:build integration

package restore_test

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// TestIntegration_UserDatabaseRestored pins issue #109: a user database that
// exists when a base backup is taken must be present in the restored data dir.
// It checks the RESTORED FILES directly (base/<oid>/) — independent of any WAL
// recovery — so backup-capture + restore-materialise is verified in isolation
// from the recovery_target behaviour (a plain restore recovers only to the
// backup's consistency point, so a database created AFTER the backup is a
// separate, expected case). Closes a coverage gap: nothing previously asserted
// that a user database survives a backup/restore round-trip.
func TestIntegration_UserDatabaseRestored(t *testing.T) {
	srv := testkit.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	// 1. Create a user database "demo" with a users table + 102 rows.
	admin, err := pg.Connect(ctx, srv.DSN, pg.ModeRegular)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if r := admin.PgConn().Exec(ctx, "CREATE DATABASE demo"); r != nil {
		if _, err := r.ReadAll(); err != nil {
			t.Fatalf("CREATE DATABASE demo: %v", err)
		}
	}

	demoDSN := strings.Replace(srv.DSN, "/hsctl?", "/demo?", 1)
	demo, err := pg.Connect(ctx, demoDSN, pg.ModeRegular)
	if err != nil {
		t.Fatalf("connect demo: %v", err)
	}
	for _, stmt := range []string{
		"CREATE TABLE users (id int primary key, name text)",
		"INSERT INTO users SELECT g, 'user'||g FROM generate_series(1,102) g",
	} {
		if err := demo.PgConn().ExecParams(ctx, stmt, nil, nil, nil, nil).Read().Err; err != nil {
			t.Fatalf("demo %q: %v", stmt, err)
		}
	}
	_ = admin.PgConn().ExecParams(ctx, "CHECKPOINT", nil, nil, nil, nil).Read()

	// 2. Get demo's database OID (its base/<oid>/ directory name).
	res := admin.PgConn().ExecParams(ctx, "SELECT oid::text FROM pg_database WHERE datname='demo'", nil, nil, nil, nil).Read()
	if res.Err != nil || len(res.Rows) != 1 {
		t.Fatalf("query demo oid: err=%v rows=%d", res.Err, len(res.Rows))
	}
	demoOID := string(res.Rows[0][0])
	t.Logf("demo database OID = %s", demoOID)
	_ = admin.Close(ctx)
	_ = demo.Close(ctx)

	// 3. Base backup.
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	bres, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN, RepoURL: repoURL, Deployment: "db1",
		Signer: signer, Verifier: verifier, Fast: true, IncludeManifest: true,
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}

	// 4. Restore.
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := restore.Restore(ctx, restore.Options{
		RepoURL: repoURL, Deployment: "db1", BackupID: bres.BackupID,
		TargetDir: target, Verifier: verifier,
	}); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// 5. The demo database's base/<oid>/ directory must be present + non-empty
	//    in the restored data dir.
	demoDir := filepath.Join(target, "base", demoOID)
	entries, err := os.ReadDir(demoDir)
	if err != nil {
		baseDirs, _ := os.ReadDir(filepath.Join(target, "base"))
		var got []string
		for _, e := range baseDirs {
			got = append(got, e.Name())
		}
		t.Fatalf("user database dir base/%s missing from restored cluster: %v\nbase/ subdirs present: %v",
			demoOID, err, got)
	}
	if len(entries) == 0 {
		t.Fatalf("base/%s exists but is EMPTY in restored cluster", demoOID)
	}
	t.Logf("base/%s restored with %d files — user database present", demoOID, len(entries))
}
