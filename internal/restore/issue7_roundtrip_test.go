// L7 — issue #7 regression artefact.  Drives the full
// tarsink → manifest → restore pipeline against a synthetic
// PGDATA-like tar that includes both TypeReg files AND
// TypeDir entries for the empty PG-required directories
// (pg_wal/, pg_dynshmem/, ...).  Asserts that every input
// path lands on the restored datadir with the right type
// and content.
//
// If a future change re-introduces the original bug — tarsink
// dropping TypeDir entries, manifest omitting Dirs, restore
// skipping the dir-create loop — this test fails immediately.
// No PG, no Docker, no network; runs in ~10 ms.
package restore_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	mathrand "math/rand"
	"net/url"
	"os"
	"path/filepath"
	"sort"
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

// TestRoundTrip_Issue7_DirsAndFiles_AllSurvive — synthetic
// BASE_BACKUP-style tar fed through tarsink, manifest
// committed, restore exercised.  Exhaustive: every input
// path in the source must show up at the restored target,
// matching type (file/dir), content (for files), mode bits.
//
// PG_WAL is the headline empty dir — its absence is what
// stops PG from starting on a restored cluster.  Multiple
// other empty dirs covered for breadth.
func TestRoundTrip_Issue7_DirsAndFiles_AllSurvive(t *testing.T) {
	type fileEntry struct {
		path string
		body []byte
		mode int64
	}
	type dirEntry struct {
		path string
		mode int64
	}

	// Synthetic PGDATA shape: a handful of files at various
	// chunk-boundary sizes + every empty dir PG sends.  Mirror
	// the issue-#7 reporter's symptom — the dirs we expect
	// should ALL appear on disk after restore, including the
	// empty ones that materialize-time MkdirAll-from-parent
	// won't create.
	r := mathrand.New(mathrand.NewSource(0xCAFE))
	files := []fileEntry{
		{path: "PG_VERSION", body: []byte("17\n"), mode: 0o600},
		{path: "base/1/PG_VERSION", body: []byte("17\n"), mode: 0o600},
		{path: "base/16384/2619", body: randomBlob(r, 200_000), mode: 0o600},
		{path: "global/pg_control", body: randomBlob(r, 8192), mode: 0o600},
		{path: "global/pg_filenode.map", body: randomBlob(r, 512), mode: 0o600},
	}
	emptyDirs := []dirEntry{
		// Headline: this is what issue #7 was about.
		{path: "pg_wal", mode: 0o700},
		// The full empty-dir set PG creates in PGDATA.
		{path: "pg_dynshmem", mode: 0o700},
		{path: "pg_notify", mode: 0o700},
		{path: "pg_replslot", mode: 0o700},
		{path: "pg_serial", mode: 0o700},
		{path: "pg_snapshots", mode: 0o700},
		{path: "pg_stat", mode: 0o700},
		{path: "pg_stat_tmp", mode: 0o700},
		{path: "pg_subtrans", mode: 0o700},
		{path: "pg_tblspc", mode: 0o700},
		{path: "pg_twophase", mode: 0o700},
	}

	// Build a real BASE_BACKUP-shaped tar.  PG sends each dir
	// as a TypeDir entry with trailing slash + mode; we match
	// that shape so a regression that special-cases trailing-
	// slash names is also caught.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, d := range emptyDirs {
		if err := tw.WriteHeader(&tar.Header{
			Name:     d.path + "/",
			Mode:     d.mode,
			Typeflag: tar.TypeDir,
			ModTime:  time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name:     f.path,
			Mode:     f.mode,
			Size:     int64(len(f.body)),
			Typeflag: tar.TypeReg,
			ModTime:  time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	// PG also embeds backup_label in the first tablespace's tar.
	const backupLabel = "START WAL LOCATION: 0/3000028 (file 000000010000000000000003)\n"
	if err := tw.WriteHeader(&tar.Header{
		Name:     "backup_label",
		Mode:     0o600,
		Size:     int64(len(backupLabel)),
		Typeflag: tar.TypeReg,
		ModTime:  time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(backupLabel)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	// Wire up: filesystem-backed CAS + tarsink.
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
	sink := tarsink.New(context.Background(), cas)

	// Drive: pretend BASE_BACKUP emitted the whole tar at once.
	if err := sink.OnTablespaceStart(0, basebackup.TablespaceInfo{OID: 1663}); err != nil {
		t.Fatalf("OnTablespaceStart: %v", err)
	}
	if err := sink.OnTablespaceData(0, buf.Bytes()); err != nil {
		t.Fatalf("OnTablespaceData: %v", err)
	}
	if err := sink.OnTablespaceEnd(0); err != nil {
		t.Fatalf("OnTablespaceEnd: %v", err)
	}

	// Build + commit a manifest using whatever the sink
	// captured.  This is the assertion shape that codifies
	// issue #7 — if AllDirs() ever returns empty again, the
	// manifest will lack the dirs and the restore will lack
	// pg_wal/.
	gotDirs := sink.AllDirs()
	if len(gotDirs) != len(emptyDirs) {
		t.Fatalf("sink.AllDirs len = %d, want %d (got: %+v)",
			len(gotDirs), len(emptyDirs), gotDirs)
	}
	gotFiles := sink.AllFiles()
	if len(gotFiles) != len(files) {
		t.Fatalf("sink.AllFiles len = %d, want %d", len(gotFiles), len(files))
	}

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "db1.full.20260506T120000Z.0001",
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000007",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        time.Now().UTC(),
		StoppedAt:        time.Now().UTC(),
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files:            gotFiles,
		Dirs:             gotDirs,
		BackupLabel:      string(sink.BackupLabel()),
	}
	store := backup.NewManifestStore(sp)
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Restore.
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

	// Diff: every original file must exist with byte-identical
	// content.  Every original empty dir must exist with mode
	// 0o700.  Anything missing is a regression of issue #7.
	for _, f := range files {
		full := filepath.Join(target, f.path)
		got, err := os.ReadFile(full)
		if err != nil {
			t.Errorf("file missing on restored datadir: %s: %v", f.path, err)
			continue
		}
		if !bytes.Equal(got, f.body) {
			t.Errorf("%s: content mismatch (got %d bytes, want %d)", f.path, len(got), len(f.body))
		}
	}
	for _, d := range emptyDirs {
		full := filepath.Join(target, d.path)
		st, err := os.Stat(full)
		if err != nil {
			t.Errorf("empty dir missing on restored datadir: %s: %v — REGRESSION OF ISSUE #7", d.path, err)
			continue
		}
		if !st.IsDir() {
			t.Errorf("%s exists but is not a dir", d.path)
		}
		if mode := st.Mode().Perm(); mode != os.FileMode(d.mode) {
			t.Errorf("%s mode = %#o, want %#o", d.path, mode, d.mode)
		}
	}

	// And the headline assertion as a single failing message
	// the operator can grep CI logs for.
	if _, err := os.Stat(filepath.Join(target, "pg_wal")); err != nil {
		t.Fatal("pg_wal/ MISSING from restored datadir — issue #7 has regressed")
	}

	// Belt-and-suspenders: enumerate the restored datadir
	// top-level and ensure every input dir/file is present.
	want := map[string]bool{}
	for _, f := range files {
		// Top-level entry only — base/, global/ are dirs.
		want[topLevel(f.path)] = true
	}
	for _, d := range emptyDirs {
		want[d.path] = true
	}
	want["backup_label"] = true
	ents, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range ents {
		got[e.Name()] = true
	}
	missing := []string{}
	for k := range want {
		if !got[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("restored datadir missing top-level entries: %v", missing)
	}
}

// topLevel returns the first path segment of a relative path.
func topLevel(p string) string {
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return p
}

// randomBlob returns deterministic-per-rng bytes of len n.
func randomBlob(r *mathrand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Intn(256))
	}
	return b
}
