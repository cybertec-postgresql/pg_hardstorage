package restore_test

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// TestRestore_NonDefaultTablespace_LandsAtLocation is the end-to-end
// regression for bug #3: a file belonging to a non-default tablespace
// (its Path is relative to the tablespace root, and it carries a
// non-zero TablespaceOID) must materialise UNDER the tablespace's real
// location — the same path tablespace_map records — NOT flattened under
// PGDATA root. Before the fix, tablespace relations landed under the
// data directory while tablespace_map pointed at an empty dir: a
// silently-corrupt restore.
func TestRestore_NonDefaultTablespace_LandsAtLocation(t *testing.T) {
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	cas := repo.NewCAS(sp)

	// A real external tablespace lives at an absolute location.
	tsLocation := filepath.Join(t.TempDir(), "ts1")
	const tsOID = 16384
	const tsRelPath = "PG_17_202406281/16384/1259" // relative to tablespace root
	tsBody := []byte("tablespace relation bytes")
	pgVersionBody := []byte("17\n")

	put := func(b []byte) backup.ChunkRef {
		info, err := cas.PutChunk(context.Background(), b)
		if err != nil {
			t.Fatalf("put chunk: %v", err)
		}
		return backup.ChunkRef{Hash: info.Hash, Offset: 0, Len: info.Size}
	}

	entries := []backup.FileEntry{
		// Default-tablespace file (OID 0) → under TargetDir.
		{Path: "PG_VERSION", Size: int64(len(pgVersionBody)), Mode: 0o600, Chunks: []backup.ChunkRef{put(pgVersionBody)}},
		// Non-default tablespace file (OID 16384) → under tsLocation.
		{Path: tsRelPath, Size: int64(len(tsBody)), Mode: 0o600, TablespaceOID: tsOID, Chunks: []backup.ChunkRef{put(tsBody)}},
	}

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "db1.full.20260620T120000Z.0001",
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000042",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        time.Now().UTC(),
		StoppedAt:        time.Now().UTC(),
		Tablespaces: []backup.Tablespace{
			{OID: 1663, Location: "pg_default"}, // pseudo default (non-absolute)
			{OID: tsOID, Location: tsLocation},  // real external tablespace
		},
		Files: entries,
		Dirs: []backup.DirEntry{
			{Path: "pg_wal", Mode: 0o700},
			// An empty dir inside the tablespace must land under tsLocation too.
			{Path: "PG_17_202406281", Mode: 0o700, TablespaceOID: tsOID},
		},
		BackupLabel:   "START WAL LOCATION: 0/3000028 (file 000000010000000000000003)\n",
		TablespaceMap: "16384 " + tsLocation + "\n",
	}
	store := backup.NewManifestStore(sp)
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	target := filepath.Join(t.TempDir(), "restored")
	if _, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    repoURL,
		Deployment: "db1",
		BackupID:   m.BackupID,
		TargetDir:  target,
		Verifier:   verifier,
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// The tablespace relation must be at its REAL location.
	tsFull := filepath.Join(tsLocation, tsRelPath)
	got, err := os.ReadFile(tsFull)
	if err != nil {
		t.Fatalf("tablespace file not at its location %s: %v", tsFull, err)
	}
	if string(got) != string(tsBody) {
		t.Errorf("tablespace file content mismatch at %s", tsFull)
	}
	// It must NOT be flattened under PGDATA root (the bug).
	if _, err := os.Stat(filepath.Join(target, tsRelPath)); err == nil {
		t.Errorf("tablespace file was flattened under PGDATA root %s (bug #3 regressed)", filepath.Join(target, tsRelPath))
	}
	// The empty tablespace dir must be under the location, not PGDATA.
	if _, err := os.Stat(filepath.Join(tsLocation, "PG_17_202406281")); err != nil {
		t.Errorf("tablespace dir not recreated under its location: %v", err)
	}
	// The default-tablespace file stays under PGDATA root.
	if _, err := os.ReadFile(filepath.Join(target, "PG_VERSION")); err != nil {
		t.Errorf("default-tablespace PG_VERSION missing from target: %v", err)
	}
	// tablespace_map on disk records the same location we wrote to.
	tm, _ := os.ReadFile(filepath.Join(target, "tablespace_map"))
	if string(tm) != "16384 "+tsLocation+"\n" {
		t.Errorf("tablespace_map = %q, want the location we materialised into", tm)
	}
}
