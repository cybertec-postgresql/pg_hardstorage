package restore_test

// resume_revalidation_test.go — the crash-consistency guard that
// re-materialises a "completed" file the filesystem actually lost.
//
// An interrupted restore leaves a checkpoint listing the files it
// finished. Resume skips those files to avoid re-fetching their chunks.
// But "the checkpoint says done" is not "the bytes are on disk": after
// an OS crash / power loss the journal can persist the checkpoint's
// rename while dropping the dentry of a file fsynced into another
// directory whose parent was never dir-fsynced. Blindly trusting the
// checkpoint then yields a datadir with a missing or truncated relation
// file — one that boots (its verify leg is skipped for TDE /
// pre-manifest sources) and serves silently-wrong data.
//
// restore.Restore defends with a stat + size revalidation before every
// skip (restore.go ~L489): if the "completed" file is absent or the
// wrong size, it drops the claim and re-materialises. That revalidation
// had no end-to-end test — its unit is a stat call, but the property
// that matters is "a resume over a filesystem-lost file heals it," and
// only a full Restore proves that. Both failure shapes are pinned:
// missing, and present-but-truncated.

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	mathrand "math/rand"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/tarsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/basebackup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// buildResumeBackup commits a small but real chunked backup (tarsink →
// manifest → commit) and returns the repo URL, verifier, manifest, and
// the raw body of the one multi-chunk relation file the seal targets.
func buildResumeBackup(t *testing.T) (repoURL string, verifier *backup.Verifier, m *backup.Manifest, relPath string, relBody []byte) {
	t.Helper()
	r := mathrand.New(mathrand.NewSource(0x5EA1))
	relPath = "base/16384/2619"
	relBody = randomBlob(r, 300_000) // several chunks, so a truncation is unambiguous

	files := []struct {
		path string
		body []byte
	}{
		{"PG_VERSION", []byte("17\n")},
		{"global/pg_control", randomBlob(r, 8192)},
		{relPath, relBody},
	}
	const backupLabel = "START WAL LOCATION: 0/3000028 (file 000000010000000000000003)\n"

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: f.path, Mode: 0o600, Size: int64(len(f.body)),
			Typeflag: tar.TypeReg, ModTime: time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "backup_label", Mode: 0o600, Size: int64(len(backupLabel)),
		Typeflag: tar.TypeReg, ModTime: time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(backupLabel)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	repoURL = "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	cas := repo.NewCAS(sp)
	sink := tarsink.New(context.Background(), cas)
	if err := sink.OnTablespaceStart(0, basebackup.TablespaceInfo{OID: 1663}); err != nil {
		t.Fatal(err)
	}
	if err := sink.OnTablespaceData(0, buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := sink.OnTablespaceEnd(0); err != nil {
		t.Fatal(err)
	}

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ = backup.LoadVerifier(pub)

	m = &backup.Manifest{
		Schema: backup.Schema, BackupID: "db1.full.20260506T120000Z.0001",
		Deployment: "db1", Tenant: "default", Type: backup.BackupTypeFull,
		PGVersion: 17, SystemIdentifier: "7000000000000000007",
		StartLSN: "0/3000028", StopLSN: "0/30001A0", Timeline: 1,
		StartedAt: time.Now().UTC(), StoppedAt: time.Now().UTC(),
		Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files:       sink.AllFiles(), Dirs: sink.AllDirs(),
		BackupLabel: string(sink.BackupLabel()),
	}
	if err := backup.NewManifestStore(sp).Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return repoURL, verifier, m, relPath, relBody
}

// entryFor finds the committed FileEntry for a path and returns the
// production checkpoint key (tablespace-oid \x00 path) for it, so the
// planted "completed" claim keys exactly as the resume loop compares.
func entryFor(t *testing.T, m *backup.Manifest, path string) (backup.FileEntry, string) {
	t.Helper()
	for _, f := range m.Files {
		if f.Path == path {
			return f, fmt.Sprintf("%d\x00%s", f.TablespaceOID, f.Path)
		}
	}
	t.Fatalf("no FileEntry for %q in committed manifest", path)
	return backup.FileEntry{}, ""
}

// plantResumeCheckpoint writes a checkpoint for THIS backup that marks
// `key` complete — the state a crashed restore leaves behind.
func plantResumeCheckpoint(t *testing.T, target string, m *backup.Manifest, fe backup.FileEntry, key string) {
	t.Helper()
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	cw := restore.NewCheckpointWriter(target, restore.Checkpoint{
		BackupID: m.BackupID, Deployment: m.Deployment,
	}, 0)
	if err := cw.MarkFileDone(key, fe.Size, len(fe.Chunks)); err != nil {
		t.Fatal(err)
	}
	if err := cw.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, restore.CheckpointFilename)); err != nil {
		t.Fatalf("checkpoint not planted: %v", err)
	}
}

func TestRestore_ResumeRematerialisesFilesystemLostFile(t *testing.T) {
	// A "completed" file the crash dropped entirely. Resume must NOT
	// trust the checkpoint and skip it — it must re-materialise.
	t.Run("missing_file", func(t *testing.T) {
		repoURL, verifier, m, relPath, relBody := buildResumeBackup(t)
		fe, key := entryFor(t, m, relPath)

		target := filepath.Join(t.TempDir(), "restored")
		plantResumeCheckpoint(t, target, m, fe, key)
		// The relation file is absent — exactly the dentry the journal lost.
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(relPath))); !os.IsNotExist(err) {
			t.Fatalf("precondition: %s must be absent before restore, got err=%v", relPath, err)
		}

		if _, err := restore.Restore(context.Background(), restore.Options{
			RepoURL: repoURL, Deployment: "db1", BackupID: m.BackupID,
			TargetDir: target, Verifier: verifier,
		}); err != nil {
			t.Fatalf("Restore: %v", err)
		}

		got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(relPath)))
		if err != nil {
			t.Fatalf("resume trusted the checkpoint and skipped a filesystem-lost file — %s "+
				"is STILL missing after restore: %v.\nThe restored datadir would boot with a "+
				"missing relation file and serve silently-wrong data.", relPath, err)
		}
		if !bytes.Equal(got, relBody) {
			t.Fatalf("%s re-materialised but content is wrong: got %d bytes, want %d",
				relPath, len(got), len(relBody))
		}
	})

	// A "completed" file the crash left half-written (right name, wrong
	// size). The stat+size revalidation must catch the size mismatch and
	// heal it back to the full, correct bytes.
	t.Run("truncated_file", func(t *testing.T) {
		repoURL, verifier, m, relPath, relBody := buildResumeBackup(t)
		fe, key := entryFor(t, m, relPath)

		target := filepath.Join(t.TempDir(), "restored")
		plantResumeCheckpoint(t, target, m, fe, key)
		// A torn write: correct path, half the bytes, wrong content.
		dst := filepath.Join(target, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, relBody[:len(relBody)/2], 0o600); err != nil {
			t.Fatal(err)
		}

		if _, err := restore.Restore(context.Background(), restore.Options{
			RepoURL: repoURL, Deployment: "db1", BackupID: m.BackupID,
			TargetDir: target, Verifier: verifier,
		}); err != nil {
			t.Fatalf("Restore: %v", err)
		}

		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("read restored %s: %v", relPath, err)
		}
		if !bytes.Equal(got, relBody) {
			t.Fatalf("resume trusted the checkpoint over a truncated file — %s left at %d bytes, "+
				"want the full %d. The size revalidation did not heal the torn write; the "+
				"datadir would carry a half-written relation file.", relPath, len(got), len(relBody))
		}
	})
}
