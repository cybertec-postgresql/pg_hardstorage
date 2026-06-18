package restore_test

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// fixtureWithTablespaceMap builds a repo with one signed
// manifest that carries a non-empty TablespaceMap body. The
// remap test asserts the body lands at the target with paths
// rewritten according to the operator's mapping.
func fixtureWithTablespaceMap(t *testing.T, mapBody string) *fixture {
	t.Helper()
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

	// One small file is enough — we're testing the remap, not
	// the chunker.
	body := []byte("17\n")
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "db1.full.tsmap-test",
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        170000,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        time.Now().UTC(),
		StoppedAt:        time.Now().UTC(),
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{
			{Path: "PG_VERSION", Size: int64(len(body)),
				Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: int64(len(body))}}},
		},
		BackupLabel:   "START WAL LOCATION: 0/3000028\n",
		TablespaceMap: mapBody,
	}
	store := backup.NewManifestStore(sp)
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return &fixture{repoURL: repoURL, verifier: verifier}
}

// TestRestore_TablespaceRemap_RewritesPaths: a manifest with
// a non-empty tablespace_map body, restored with a remap
// covering one of two entries, lands at TargetDir with the
// matching path rewritten + the other untouched.
func TestRestore_TablespaceRemap_RewritesPaths(t *testing.T) {
	mapBody := "1663 /mnt/ssd/ts_fast\n1664 /mnt/hdd/ts_archive\n"
	fx := fixtureWithTablespaceMap(t, mapBody)
	target := t.TempDir() + "/restored"

	remap, err := restore.ParseTablespaceRemap([]string{
		"/mnt/ssd/ts_fast=/var/lib/pg/ts_fast",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:         fx.repoURL,
		Deployment:      "db1",
		BackupID:        "db1.full.tsmap-test",
		TargetDir:       target,
		Verifier:        fx.verifier,
		TablespaceRemap: remap,
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(target, "tablespace_map"))
	if err != nil {
		t.Fatalf("read tablespace_map: %v", err)
	}
	want := "1663 /var/lib/pg/ts_fast\n1664 /mnt/hdd/ts_archive\n"
	if string(got) != want {
		t.Errorf("tablespace_map = %q\nwant %q", got, want)
	}
}

// TestRestore_TablespaceRemap_NoRemap_PreservesBody:
// regression — without a remap, the manifest's tablespace_map
// body is written verbatim.
func TestRestore_TablespaceRemap_NoRemap_PreservesBody(t *testing.T) {
	mapBody := "1663 /mnt/ssd/ts_fast\n"
	fx := fixtureWithTablespaceMap(t, mapBody)
	target := t.TempDir() + "/restored"

	if _, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.tsmap-test",
		TargetDir:  target,
		Verifier:   fx.verifier,
		// No TablespaceRemap.
	}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(target, "tablespace_map"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != mapBody {
		t.Errorf("tablespace_map should be verbatim without remap; got %q\nwant %q", got, mapBody)
	}
}

// TestRestore_TablespaceRemap_OnlyMatchingPaths: a remap
// whose OLD path doesn't appear in the manifest body is
// effectively a no-op for that entry — the body is unchanged.
func TestRestore_TablespaceRemap_OnlyMatchingPaths(t *testing.T) {
	mapBody := "1663 /mnt/ssd/ts_fast\n"
	fx := fixtureWithTablespaceMap(t, mapBody)
	target := t.TempDir() + "/restored"

	remap, _ := restore.ParseTablespaceRemap([]string{
		// OLD doesn't match any path in the manifest.
		"/some/unrelated/path=/var/lib/pg/unrelated",
	})

	if _, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:         fx.repoURL,
		Deployment:      "db1",
		BackupID:        "db1.full.tsmap-test",
		TargetDir:       target,
		Verifier:        fx.verifier,
		TablespaceRemap: remap,
	}); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(filepath.Join(target, "tablespace_map"))
	if string(got) != mapBody {
		t.Errorf("non-matching remap should leave body verbatim; got %q\nwant %q", got, mapBody)
	}
}

// TestRestore_TablespaceRemap_EmptyManifestMap_NoFile:
// regression — when the manifest has no tablespace_map (the
// no-tablespaces case), no file is written, regardless of the
// operator's --tablespace-mapping flags.
func TestRestore_TablespaceRemap_EmptyManifestMap_NoFile(t *testing.T) {
	fx := fixtureWithTablespaceMap(t, "") // empty body
	target := t.TempDir() + "/restored"

	remap, _ := restore.ParseTablespaceRemap([]string{"/a=/b"})
	if _, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:         fx.repoURL,
		Deployment:      "db1",
		BackupID:        "db1.full.tsmap-test",
		TargetDir:       target,
		Verifier:        fx.verifier,
		TablespaceRemap: remap,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(target, "tablespace_map")); err == nil {
		t.Errorf("tablespace_map should NOT be written when manifest body is empty")
	} else if !strings.Contains(err.Error(), "no such file") {
		t.Errorf("unexpected stat error: %v", err)
	}
}
