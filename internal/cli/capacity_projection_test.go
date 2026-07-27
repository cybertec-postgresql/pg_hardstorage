package cli

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	repoPkg "github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// The pre-backup ENOSPC gate projected the next backup from the
// latest manifest BY TIMESTAMP, blind to Type. On the standard
// daily-incremental / weekly-full cadence that meant: the weekly
// FULL was projected at the incremental's ~2% size (gate
// false-passes, backup aborts mid-run on ENOSPC hours later), and
// the daily INCREMENTAL was projected at the full's size (gate
// falsely REFUSES cheap backups on a repo with ample space).
func TestProjectedBytes_TypeAware(t *testing.T) {
	root := t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: "file://" + root}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	priv, pub, err := backup.GenerateKeypair(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	store := backup.NewManifestStore(sp)

	commit := func(id, parent string, btype backup.BackupType, size int64, ts time.Time) {
		t.Helper()
		m := &backup.Manifest{
			Schema: backup.Schema, BackupID: id, Deployment: "db1",
			Type: btype, ParentBackupID: parent,
			PGVersion: 17, SystemIdentifier: "7000000000000000001",
			StartLSN: "0/3000028", StopLSN: "0/30001A0", Timeline: 1,
			StartedAt: ts.Add(-time.Minute), StoppedAt: ts,
			BackupLabel: "START WAL LOCATION: 0/3000028\n",
			Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
			Files: []backup.FileEntry{{Path: "data/f", Size: size, Mode: 0o600,
				Chunks: []backup.ChunkRef{{Hash: repoPkg.HashOf([]byte(id)), Offset: 0, Len: size}}}},
		}
		if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
			t.Fatalf("commit %s: %v", id, err)
		}
	}

	// Sunday 500 MB full, then Saturday 5 MB incremental (newest).
	commit("db1.full.20260719T020000Z.aaaa", "", backup.BackupTypeFull, 500<<20, time.Date(2026, 7, 19, 2, 0, 0, 0, time.UTC))
	commit("db1.incr.20260725T020000Z.bbbb", "db1.full.20260719T020000Z.aaaa", backup.BackupTypeIncremental, 5<<20, time.Date(2026, 7, 25, 2, 0, 0, 0, time.UTC))

	full, err := projectedBytesFromDeployment(context.Background(), sp, "db1", verifier, false)
	if err != nil {
		t.Fatal(err)
	}
	if full != 500<<20 {
		t.Errorf("full-run projection = %d, want %d (the full's size) — a small projection false-passes the ENOSPC gate and the weekly full dies mid-run", full, int64(500<<20))
	}

	incr, err := projectedBytesFromDeployment(context.Background(), sp, "db1", verifier, true)
	if err != nil {
		t.Fatal(err)
	}
	if incr != 5<<20 {
		t.Errorf("incremental-run projection = %d, want %d (the incremental's size) — over-projection falsely refuses cheap scheduled backups", incr, int64(5<<20))
	}
}
