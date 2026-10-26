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

func TestWithMapAll_ExpandsEveryExternalTablespaceDeterministically(t *testing.T) {
	tss := []backup.Tablespace{
		{OID: 1663, Location: "pg_default"}, // pseudo tablespace: untouched
		{OID: 0, Location: ""},              // main data dir: untouched
		{OID: 16400, Location: "/mnt/ssd/a"},
		{OID: 16401, Location: "/mnt/hdd/b"},
		{OID: 16402, Location: "/mnt/hdd/added_later"},
	}
	explicit, err := restore.ParseTablespaceRemap([]string{"/mnt/hdd/b=/elsewhere/b"})
	if err != nil {
		t.Fatal(err)
	}
	got := explicit.WithMapAll("/restore/ts", tss)
	want := map[string]string{
		"/mnt/hdd/b":           "/elsewhere/b", // explicit entry wins
		"/mnt/ssd/a":           "/restore/ts/16400",
		"/mnt/hdd/added_later": "/restore/ts/16402",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries %+v, want %d", len(got), got, len(want))
	}
	for _, e := range got {
		if want[e.Old] != e.New {
			t.Errorf("%s -> %s, want %s", e.Old, e.New, want[e.Old])
		}
	}
	// Same input, same output.
	again := explicit.WithMapAll("/restore/ts", tss)
	for i := range got {
		if got[i] != again[i] {
			t.Errorf("not deterministic at %d: %+v vs %+v", i, got[i], again[i])
		}
	}
	// No parent: unchanged.
	if g := explicit.WithMapAll("", tss); len(g) != 1 {
		t.Errorf("empty parent changed the remap: %+v", g)
	}
}

func TestCleanTablespaceMapAll(t *testing.T) {
	if got, err := restore.CleanTablespaceMapAll(""); err != nil || got != "" {
		t.Errorf("empty = %q, %v", got, err)
	}
	if got, err := restore.CleanTablespaceMapAll("/a//b/../c/"); err != nil || got != "/a/c" {
		t.Errorf("clean = %q, %v", got, err)
	}
	for _, bad := range []string{"relative/dir", "/ok\n99999 /evil", "/nul\x00"} {
		if _, err := restore.CleanTablespaceMapAll(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// End to end: a backup with two external tablespaces restores both under
// the one parent, an explicit mapping wins, and tablespace_map plus the
// pg_tblspc links point where the bytes were written.
func TestRestore_TablespaceMapAll_RedirectsEveryTablespace(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(ctx, storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	cas := repo.NewCAS(sp)
	put := func(b []byte) backup.ChunkRef {
		ci, err := cas.PutChunk(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		return backup.ChunkRef{Hash: ci.Hash, Len: int64(len(b))}
	}
	pgv, a, b := []byte("17\n"), []byte("tablespace a data"), []byte("tablespace b data")
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	srcA, srcB := "/mnt/ssd/ts_a", "/mnt/hdd/ts_b"
	now := time.Now().UTC()
	m := &backup.Manifest{
		Schema: backup.Schema, BackupID: "db1.full.mapall", Deployment: "db1", Tenant: "default",
		Type: backup.BackupTypeFull, PGVersion: 170000, SystemIdentifier: "7000000000000000001",
		StartLSN: "0/3000028", StopLSN: "0/30001A0", Timeline: 1, StartedAt: now, StoppedAt: now,
		BackupLabel:   "START WAL LOCATION: 0/3000028\n",
		Tablespaces:   []backup.Tablespace{{OID: 16400, Location: srcA}, {OID: 16401, Location: srcB}},
		TablespaceMap: "16400 " + srcA + "\n16401 " + srcB + "\n",
		Files: []backup.FileEntry{
			{Path: "PG_VERSION", Size: int64(len(pgv)), Chunks: []backup.ChunkRef{func() backup.ChunkRef { c := put(pgv); return c }()}},
			{Path: "PG_17_1/5/1234", Size: int64(len(a)), TablespaceOID: 16400, Chunks: []backup.ChunkRef{put(a)}},
			{Path: "PG_17_1/5/5678", Size: int64(len(b)), TablespaceOID: 16401, Chunks: []backup.ChunkRef{put(b)}},
		},
	}
	plantArchivedWAL(t, sp, "db1", 1)
	if err := backup.NewManifestStore(sp).Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	scratch := t.TempDir()
	parent := filepath.Join(scratch, "tablespaces")
	explicitB := filepath.Join(scratch, "explicit_b")
	remap, err := restore.ParseTablespaceRemap([]string{srcB + "=" + explicitB})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(scratch, "pgdata")
	if _, err := restore.Restore(ctx, restore.Options{
		RepoURL: repoURL, Deployment: "db1", BackupID: "db1.full.mapall", TargetDir: target,
		Verifier: verifier, TablespaceRemap: remap, TablespaceMapAll: parent,
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	wantA := filepath.Join(parent, "16400")
	for path, body := range map[string][]byte{
		filepath.Join(wantA, "PG_17_1/5/1234"):     a,
		filepath.Join(explicitB, "PG_17_1/5/5678"): b,
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(body) {
			t.Errorf("%s = %q, %v; want %q", path, got, err, body)
		}
	}
	if _, err := os.Stat(filepath.Join(parent, "16401")); err == nil {
		t.Errorf("explicit mapping lost: tablespace 16401 also landed under the parent")
	}
	tsmap, err := os.ReadFile(filepath.Join(target, "tablespace_map"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "16400 " + wantA + "\n16401 " + explicitB + "\n"; string(tsmap) != want {
		t.Errorf("tablespace_map = %q, want %q", tsmap, want)
	}
	if dest, err := os.Readlink(filepath.Join(target, "pg_tblspc", "16400")); err != nil || dest != wantA {
		t.Errorf("pg_tblspc/16400 -> %q, %v; want %q", dest, err, wantA)
	}

	// A relative parent is a usage error, not a silent no-op.
	_, err = restore.Restore(ctx, restore.Options{
		RepoURL: repoURL, Deployment: "db1", BackupID: "db1.full.mapall",
		TargetDir: filepath.Join(scratch, "pgdata2"), Verifier: verifier, TablespaceMapAll: "relative",
	})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative parent: err = %v", err)
	}
}
