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

// Regression (M99): resuming an interrupted restore of a backup with a
// non-default tablespace was refused — the tablespace dir already held
// the files the first attempt wrote, and preflightTablespaceTargets
// had no resume exemption (preflight.tablespace_not_empty). With
// --force it was worse: the dir was wiped, while the checkpoint still
// listed those files as done, so the resume skipped them and the
// restore "succeeded" without them.
func TestRestore_ResumeWithNonDefaultTablespace(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "force"}[force], func(t *testing.T) {
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
			cas := repo.NewCAS(sp)
			put := func(b []byte) backup.ChunkRef {
				info, err := cas.PutChunk(context.Background(), b)
				if err != nil {
					t.Fatal(err)
				}
				return backup.ChunkRef{Hash: info.Hash, Offset: 0, Len: info.Size}
			}

			tsLocation := filepath.Join(t.TempDir(), "ts1")
			const tsOID = 16384
			const tsRelPath = "PG_17_202406281/16384/1259"
			tsBody := []byte("tablespace relation bytes")
			pgv := []byte("17\n")

			priv, pub, _ := backup.GenerateKeypair(rand.Reader)
			signer, _ := backup.LoadSigner(priv)
			verifier, _ := backup.LoadVerifier(pub)
			m := &backup.Manifest{
				Schema: backup.Schema, BackupID: "db1.full.20260620T120000Z.0001", Deployment: "db1",
				Tenant: "default", Type: backup.BackupTypeFull, PGVersion: 17,
				SystemIdentifier: "7000000000000000042", StartLSN: "0/3000028", StopLSN: "0/30001A0",
				Timeline: 1, StartedAt: time.Now().UTC(), StoppedAt: time.Now().UTC(),
				Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}, {OID: tsOID, Location: tsLocation}},
				Files: []backup.FileEntry{
					{Path: "PG_VERSION", Size: int64(len(pgv)), Mode: 0o600, Chunks: []backup.ChunkRef{put(pgv)}},
					{Path: tsRelPath, Size: int64(len(tsBody)), Mode: 0o600, TablespaceOID: tsOID, Chunks: []backup.ChunkRef{put(tsBody)}},
				},
				BackupLabel:   "START WAL LOCATION: 0/3000028 (file 000000010000000000000003)\n",
				TablespaceMap: "16384 " + tsLocation + "\n",
			}
			plantArchivedWAL(t, sp, m.Deployment, m.Timeline)
			if err := backup.NewManifestStore(sp).Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
				t.Fatal(err)
			}

			// The interrupted first attempt: the tablespace file is on
			// disk and checkpointed; PG_VERSION is not yet written.
			target := filepath.Join(t.TempDir(), "restored")
			if err := os.MkdirAll(filepath.Join(tsLocation, filepath.Dir(tsRelPath)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(tsLocation, tsRelPath), tsBody, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatal(err)
			}
			cw := restore.NewCheckpointWriter(target, restore.Checkpoint{
				BackupID: m.BackupID, Deployment: "db1", TargetDir: target, StartedAt: time.Now().UTC(),
			}, 1)
			if err := cw.MarkFileDone(tsRelPath, int64(len(tsBody)), 1); err != nil {
				t.Fatal(err)
			}
			if err := cw.Flush(); err != nil {
				t.Fatal(err)
			}

			if _, err := restore.Restore(context.Background(), restore.Options{
				RepoURL: repoURL, Deployment: "db1", BackupID: m.BackupID,
				TargetDir: target, Verifier: verifier, AllowOverwrite: force,
			}); err != nil {
				t.Fatalf("resume of an interrupted tablespace restore: %v", err)
			}
			if got, err := os.ReadFile(filepath.Join(tsLocation, tsRelPath)); err != nil || string(got) != string(tsBody) {
				t.Errorf("tablespace file after resume: %q, %v (want it intact)", got, err)
			}
			if _, err := os.Stat(filepath.Join(target, "PG_VERSION")); err != nil {
				t.Errorf("resume did not finish the default tablespace: %v", err)
			}
		})
	}
}
