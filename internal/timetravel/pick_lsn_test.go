package timetravel

import (
	"context"
	"crypto/rand"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// LSN-mode timetravel used to pick "the latest committed backup
// overall and let PG's own recovery decide" — but PG can only roll
// FORWARD, so any target LSN below the newest backup's stop_lsn (the
// entire point of timetravel) hit the restore reachability gate as
// target_unreachable while the correct older backup sat unused. The
// picker must select by StopLSN <= target.
func TestPickBackupForTarget_LSNSelectsOlderBackup(t *testing.T) {
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	store := backup.NewManifestStore(sp)

	commit := func(id, stopLSN string, ts time.Time) {
		t.Helper()
		m := &backup.Manifest{
			Schema: backup.Schema, BackupID: id, Deployment: "db1",
			Type: backup.BackupTypeFull, PGVersion: 17,
			SystemIdentifier: "7000000000000000001",
			StartLSN:         "0/1000028", StopLSN: stopLSN, Timeline: 1,
			StartedAt: ts.Add(-time.Minute), StoppedAt: ts,
			BackupLabel: "START WAL LOCATION: 0/1000028\n",
			Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
			Files:       []backup.FileEntry{},
		}
		if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
			t.Fatalf("commit %s: %v", id, err)
		}
	}

	now := time.Now().UTC()
	commit("db1.full.old", "0/5000000", now.Add(-2*time.Hour))
	commit("db1.full.new", "0/9000000", now.Add(-1*time.Hour))

	mgr := NewManager(filepath.Join(t.TempDir(), "state.json"), "/usr/bin/true")

	// Target between the two stops → MUST pick the older backup.
	id, _, err := mgr.pickBackupForTarget(context.Background(), repoURL, "db1", time.Time{}, "0/6000000", verifier)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if id != "db1.full.old" {
		t.Errorf("picked %q for LSN 0/6000000, want db1.full.old — the newer backup ends past the target and PG cannot rewind", id)
	}

	// Target past both stops → the newest works.
	id, _, err = mgr.pickBackupForTarget(context.Background(), repoURL, "db1", time.Time{}, "0/A000000", verifier)
	if err != nil {
		t.Fatalf("pick past-both: %v", err)
	}
	if id != "db1.full.new" {
		t.Errorf("picked %q for LSN 0/A000000, want db1.full.new", id)
	}

	// Target before every stop → structured refusal, not a wrong pick.
	_, _, err = mgr.pickBackupForTarget(context.Background(), repoURL, "db1", time.Time{}, "0/2000000", verifier)
	if err == nil || !strings.Contains(err.Error(), "at-or-before LSN") {
		t.Errorf("too-early target: err = %v, want at-or-before refusal", err)
	}
}
