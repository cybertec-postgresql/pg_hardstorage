package restore_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	mathrand "math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// fixture builds a fresh repo with one signed manifest pointing at a
// known set of chunks. Returns the repo URL, the file bodies that the
// manifest references (so tests can assert byte-equality), the
// verifier, and a teardown.
type fixture struct {
	repoURL  string
	verifier *backup.Verifier
	files    map[string][]byte // path -> body
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	repoURL := "file://" + root

	// 1. Init the repo.
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}

	// 2. Open storage + CAS for chunk writes.
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	cas := repo.NewCAS(sp)

	// 3. Build a small set of files with various chunk counts.
	r := mathrand.New(mathrand.NewSource(0xC0FFEE))
	files := map[string][]byte{
		"PG_VERSION":        []byte("17\n"),
		"empty_marker":      {},
		"base/16384/2619":   randomBytes(r, 200_000), // multi-chunk
		"global/pg_control": randomBytes(r, 8192),    // exactly one page
	}

	var entries []backup.FileEntry
	for path, body := range files {
		entry := backup.FileEntry{
			Path: path,
			Size: int64(len(body)),
			Mode: 0o600,
		}
		// Single-chunk per file (sufficient for unit-test purposes;
		// real chunker exercising lives in chunker tests).
		if len(body) > 0 {
			info, err := cas.PutChunk(context.Background(), body)
			if err != nil {
				t.Fatalf("put chunk for %s: %v", path, err)
			}
			entry.Chunks = []backup.ChunkRef{{
				Hash:   info.Hash,
				Offset: 0,
				Len:    info.Size,
			}}
		}
		entries = append(entries, entry)
	}

	// 4. Sign + commit a manifest.
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "db1.full.20260428T130000Z.0001",
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        time.Now().UTC(),
		StoppedAt:        time.Now().UTC(),
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files:            entries,
		// Dirs covers the empty PG-required dirs that PG sends as
		// tar TypeDir entries.  Restore must MkdirAll each one so
		// PG can start on the restored datadir.  See issue #7.
		Dirs: []backup.DirEntry{
			{Path: "pg_wal", Mode: 0o700},
			{Path: "pg_replslot", Mode: 0o700},
			{Path: "pg_dynshmem", Mode: 0o700},
		},
		BackupLabel:   "START WAL LOCATION: 0/3000028 (file 000000010000000000000003)\n",
		TablespaceMap: "",
	}
	store := backup.NewManifestStore(sp)
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return &fixture{repoURL: repoURL, verifier: verifier, files: files}
}

func randomBytes(r *mathrand.Rand, n int) []byte {
	b := make([]byte, n)
	r.Read(b)
	return b
}

func TestRestore_RoundTrip(t *testing.T) {
	fx := newFixture(t)
	target := t.TempDir() + "/restored"

	res, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.20260428T130000Z.0001",
		TargetDir:  target,
		Verifier:   fx.verifier,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.FileCount != len(fx.files) {
		t.Errorf("FileCount = %d, want %d", res.FileCount, len(fx.files))
	}
	if res.Duration <= 0 {
		t.Error("Duration not populated")
	}

	// Each file must reconstitute byte-for-byte.
	for path, want := range fx.files {
		got, err := os.ReadFile(filepath.Join(target, path))
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s mismatch (len got %d, want %d)", path, len(got), len(want))
		}
	}

	// backup_label must exist.
	got, err := os.ReadFile(filepath.Join(target, "backup_label"))
	if err != nil {
		t.Errorf("backup_label not written: %v", err)
	}
	if !strings.Contains(string(got), "START WAL LOCATION") {
		t.Errorf("backup_label content: %q", got)
	}
	// tablespace_map should be absent when manifest field is empty.
	if _, err := os.Stat(filepath.Join(target, "tablespace_map")); err == nil {
		t.Error("tablespace_map should NOT be written when manifest field is empty")
	}
}

// TestRestore_EmptyDirs_AreRecreated regression-tests issue
// #7 — without manifest.Dirs being honoured, empty PG-required
// directories like pg_wal/ are missing on the restored datadir
// and PG refuses to start.  The fixture seeds the manifest
// with three such dirs; this test asserts they all land on
// disk with the expected mode.
func TestRestore_EmptyDirs_AreRecreated(t *testing.T) {
	fx := newFixture(t)
	target := t.TempDir() + "/restored"

	if _, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.20260428T130000Z.0001",
		TargetDir:  target,
		Verifier:   fx.verifier,
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	for _, d := range []string{"pg_wal", "pg_replslot", "pg_dynshmem"} {
		full := filepath.Join(target, d)
		st, err := os.Stat(full)
		if err != nil {
			t.Errorf("expected %s/ on restored datadir; got: %v", d, err)
			continue
		}
		if !st.IsDir() {
			t.Errorf("%s exists but is not a dir", d)
		}
		// Mode 0o700 expected; allow umask-trimmed lower bits.
		if mode := st.Mode().Perm(); mode != 0o700 {
			t.Errorf("%s mode = %#o, want %#o", d, mode, 0o700)
		}
	}
}

func TestRestore_RejectsForeignSigner(t *testing.T) {
	fx := newFixture(t)
	target := t.TempDir() + "/restored"

	// A different verifier — the manifest's signature is genuine but
	// the verifier we hold doesn't match.
	_, foreignPub, _ := backup.GenerateKeypair(rand.Reader)
	foreignVerifier, _ := backup.LoadVerifier(foreignPub)

	_, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.20260428T130000Z.0001",
		TargetDir:  target,
		Verifier:   foreignVerifier,
	})
	if err == nil {
		t.Fatal("restore with foreign verifier must fail")
	}
	// Confirm we did NOT create any files in target.
	if _, err := os.Stat(target); err == nil {
		entries, _ := os.ReadDir(target)
		if len(entries) > 0 {
			t.Errorf("target dir should be empty on verify failure; got %d entries", len(entries))
		}
	}
}

func TestRestore_RejectsNonEmptyTarget(t *testing.T) {
	fx := newFixture(t)
	target := t.TempDir() + "/restored"
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "stale"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.20260428T130000Z.0001",
		TargetDir:  target,
		Verifier:   fx.verifier,
	})
	if err == nil {
		t.Fatal("expected error when target is non-empty")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected structured error; got %v", err)
	}
	if oe.Code != "preflight.target_not_empty" {
		t.Errorf("code = %q", oe.Code)
	}
}

// TestRestore_RefusesPGDataDir — target with PG_VERSION
// present must surface the upgraded `preflight.target_pg_datadir`
// code so the operator sees the consequence of --force before
// reaching for it.  Without this, an off-by-one path typo
// (`/var/lib/postgresql/17/main` vs the intended
// `/var/lib/postgresql/17/restored`) silently obliterates the
// real cluster's datadir on retry.
func TestRestore_RefusesPGDataDir(t *testing.T) {
	fx := newFixture(t)
	target := t.TempDir() + "/restored"
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	// PG_VERSION is the canonical "this is a datadir"
	// marker — every PG release writes it at initdb time.
	if err := os.WriteFile(filepath.Join(target, "PG_VERSION"), []byte("17\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "postgresql.conf"), []byte("# stale config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.20260428T130000Z.0001",
		TargetDir:  target,
		Verifier:   fx.verifier,
	})
	if err == nil {
		t.Fatal("expected refusal; PG_VERSION + restore without --force must error")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected structured error; got %v", err)
	}
	if oe.Code != "preflight.target_pg_datadir" {
		t.Errorf("code = %q; want preflight.target_pg_datadir", oe.Code)
	}
	// Operator-visible suggestion must name a concrete
	// next step (the `mv` aside trick).
	if oe.Suggestion == nil || !strings.Contains(oe.Suggestion.Human, "mv "+target) {
		t.Errorf("suggestion missing mv-aside hint: %+v", oe.Suggestion)
	}
}

// TestRestore_RefusesRunningPostgres — postmaster.pid
// naming a LIVE PID is the one case --force does not
// override.  Use our own PID so the liveness check is
// deterministic.
func TestRestore_RefusesRunningPostgres(t *testing.T) {
	fx := newFixture(t)
	target := t.TempDir() + "/restored"
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	// postmaster.pid format: PID, datadir, port, socket-dir,
	// start-time, shmem-key — but only the first line
	// matters for our liveness check.
	pid := os.Getpid()
	body := strconv.Itoa(pid) + "\n" + target + "\n5432\n/tmp\n12345\n67890\n"
	if err := os.WriteFile(filepath.Join(target, "postmaster.pid"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// PG_VERSION present too — realistic shape.
	if err := os.WriteFile(filepath.Join(target, "PG_VERSION"), []byte("17\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Even with --force, a running cluster's datadir must
	// not be overwritten.
	_, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:        fx.repoURL,
		Deployment:     "db1",
		BackupID:       "db1.full.20260428T130000Z.0001",
		TargetDir:      target,
		Verifier:       fx.verifier,
		AllowOverwrite: true,
	})
	if err == nil {
		t.Fatal("expected refusal; --force must NOT override a live postmaster.pid")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected structured error; got %v", err)
	}
	if oe.Code != "preflight.target_running_postgres" {
		t.Errorf("code = %q; want preflight.target_running_postgres", oe.Code)
	}
	// Suggestion must point at `pg_ctl ... stop`.
	if oe.Suggestion == nil || !strings.Contains(oe.Suggestion.Command, "pg_ctl") {
		t.Errorf("suggestion missing pg_ctl stop hint: %+v", oe.Suggestion)
	}
}

// TestRestore_StalePostmasterPIDAllowsForce — the same
// shape as the live-PG case but with a clearly-dead PID
// (we use PID 1 and trust that signal-0 to PID 1 will
// either succeed or EPERM — both treated as "alive" — so
// instead use a high unallocated PID).  Operators
// occasionally hit this when a previous restore crashed
// without cleaning up postmaster.pid.  --force must work.
func TestRestore_StalePostmasterPIDAllowsForce(t *testing.T) {
	fx := newFixture(t)
	target := t.TempDir() + "/restored"
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	// PID 0x7FFFFFFE is past Linux's pid_max ceiling on
	// every distro we ship for; signal-0 will return ESRCH.
	body := "2147483646\n" + target + "\n5432\n/tmp\n12345\n67890\n"
	if err := os.WriteFile(filepath.Join(target, "postmaster.pid"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Without PG_VERSION present, this is just a stale lock
	// file in an otherwise random dir — --force should
	// proceed.  (If we added PG_VERSION the
	// preflight.target_pg_datadir test above already covers
	// the louder-message path.)
	_, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:        fx.repoURL,
		Deployment:     "db1",
		BackupID:       "db1.full.20260428T130000Z.0001",
		TargetDir:      target,
		Verifier:       fx.verifier,
		AllowOverwrite: true,
	})
	if err != nil {
		t.Fatalf("--force with stale postmaster.pid: %v", err)
	}
}

// TestRestore_MalformedPostmasterPID_RefusesEvenWithForce pins the
// fail-closed fix: a postmaster.pid that EXISTS but whose PID can't be
// parsed (partial write / corruption while PG is actually running)
// must refuse even with --force. Returning a 0 PID and treating it as
// "no running PG" would let --force overwrite a live cluster whose
// lockfile we merely failed to parse — the exact data loss this gate
// exists to prevent.
func TestRestore_MalformedPostmasterPID_RefusesEvenWithForce(t *testing.T) {
	fx := newFixture(t)
	target := t.TempDir() + "/restored"
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	// First line is not a parseable PID.
	body := "not-a-pid\n" + target + "\n5432\n/tmp\n12345\n67890\n"
	if err := os.WriteFile(filepath.Join(target, "postmaster.pid"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:        fx.repoURL,
		Deployment:     "db1",
		BackupID:       "db1.full.20260428T130000Z.0001",
		TargetDir:      target,
		Verifier:       fx.verifier,
		AllowOverwrite: true, // --force must NOT override an unverifiable lockfile
	})
	if err == nil {
		t.Fatal("expected refusal for an unparseable postmaster.pid even with --force")
	}
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "preflight.target_postmaster_unverifiable" {
		t.Fatalf("want preflight.target_postmaster_unverifiable; got %v", err)
	}
}

func TestRestore_AllowOverwriteSucceeds(t *testing.T) {
	fx := newFixture(t)
	target := t.TempDir() + "/restored"
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "stale"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:        fx.repoURL,
		Deployment:     "db1",
		BackupID:       "db1.full.20260428T130000Z.0001",
		TargetDir:      target,
		Verifier:       fx.verifier,
		AllowOverwrite: true,
	})
	if err != nil {
		t.Fatalf("AllowOverwrite restore: %v", err)
	}
}

func TestRestore_TargetExistsAsFile(t *testing.T) {
	fx := newFixture(t)
	parent := t.TempDir()
	target := filepath.Join(parent, "restored")
	if err := os.WriteFile(target, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.20260428T130000Z.0001",
		TargetDir:  target,
		Verifier:   fx.verifier,
	})
	if err == nil {
		t.Fatal("expected error when target exists and is not a directory")
	}
}

func TestRestore_BackupNotFound(t *testing.T) {
	fx := newFixture(t)
	target := t.TempDir() + "/restored"
	_, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   "does-not-exist",
		TargetDir:  target,
		Verifier:   fx.verifier,
	})
	if err == nil {
		t.Fatal("expected error for missing backup")
	}
}

func TestRestore_RepoNotARepo(t *testing.T) {
	emptyDir := t.TempDir()
	_, _, _ = backup.GenerateKeypair(rand.Reader)
	_, pub, _ := backup.GenerateKeypair(rand.Reader)
	verifier, _ := backup.LoadVerifier(pub)

	_, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    "file://" + emptyDir,
		Deployment: "db1",
		BackupID:   "x",
		TargetDir:  t.TempDir() + "/restored",
		Verifier:   verifier,
	})
	if err == nil {
		t.Fatal("expected error when repo doesn't exist")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected structured error; got %v", err)
	}
	if oe.Code != "notfound.repo" {
		t.Errorf("code = %q, want notfound.repo", oe.Code)
	}
}

func TestRestore_ValidateOptions(t *testing.T) {
	cases := []struct {
		name string
		opts restore.Options
		want string
	}{
		{"missing RepoURL", restore.Options{Deployment: "d", BackupID: "b", TargetDir: "/t"}, "RepoURL"},
		{"missing Deployment", restore.Options{RepoURL: "x", BackupID: "b", TargetDir: "/t"}, "Deployment"},
		{"missing BackupID", restore.Options{RepoURL: "x", Deployment: "d", TargetDir: "/t"}, "BackupID"},
		{"missing TargetDir", restore.Options{RepoURL: "x", Deployment: "d", BackupID: "b"}, "TargetDir"},
		{"missing Verifier", restore.Options{RepoURL: "x", Deployment: "d", BackupID: "b", TargetDir: "/t"}, "Verifier"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := restore.Restore(context.Background(), c.opts)
			if err == nil {
				t.Fatalf("expected error mentioning %q", c.want)
			}
			if !errors.Is(err, output.ErrUsage) {
				t.Errorf("validation error should wrap ErrUsage; got %v", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error should mention %q; got %v", c.want, err)
			}
		})
	}
}

func TestRestore_EventStream(t *testing.T) {
	fx := newFixture(t)
	target := t.TempDir() + "/restored"
	var events []*output.Event
	_, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.20260428T130000Z.0001",
		TargetDir:  target,
		Verifier:   fx.verifier,
		OnEvent:    func(e *output.Event) { events = append(events, e) },
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"restore.manifest_loaded": false,
		"restore.started":         false,
		"restore.completed":       false,
	}
	for _, ev := range events {
		key := ev.Component + "." + ev.Op
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing event: %s", k)
		}
	}
}

func TestRestore_FileMode_Preserved(t *testing.T) {
	fx := newFixture(t)
	target := t.TempDir() + "/restored"
	if _, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.20260428T130000Z.0001",
		TargetDir:  target,
		Verifier:   fx.verifier,
	}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(target, "PG_VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	// Mode 0600 was set in the fixture; confirm it survived.
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("PG_VERSION mode = %#o, want 0600", mode)
	}
}

func TestRestore_CtxCancelMidWay(t *testing.T) {
	fx := newFixture(t)
	target := t.TempDir() + "/restored"
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call

	_, err := restore.Restore(ctx, restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.20260428T130000Z.0001",
		TargetDir:  target,
		Verifier:   fx.verifier,
	})
	if !errors.Is(err, context.Canceled) {
		// repo.Open might return its own error before ctx is checked;
		// either is acceptable as long as we did NOT silently succeed.
		if err == nil {
			t.Errorf("ctx-cancelled restore should not succeed silently")
		}
	}
}

// newTDEFixture builds the same manifest shape newFixture does but
// stamps Manifest.SourceTDE so the restore-side TDE relaxation
// fires.  The repo + chunks + signing keys are independent — the
// two fixtures don't interfere if used in the same test binary.
func newTDEFixture(t *testing.T) *fixture {
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

	files := map[string][]byte{
		"PG_VERSION":        []byte("17\n"),
		"global/pg_control": bytes.Repeat([]byte{0xAB}, 8192),
	}
	var entries []backup.FileEntry
	for path, body := range files {
		info, err := cas.PutChunk(context.Background(), body)
		if err != nil {
			t.Fatalf("put chunk %s: %v", path, err)
		}
		entries = append(entries, backup.FileEntry{
			Path: path, Size: int64(len(body)), Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: info.Size}},
		})
	}

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "tde-db.full.20260527T120000Z.0001",
		Deployment:       "tde-db",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000999",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        time.Now().UTC(),
		StoppedAt:        time.Now().UTC(),
		// The point of this fixture: source PG was declared TDE.
		// Restore-side should see SourceTDE != nil and skip the
		// in-process pg_verifybackup gate.
		SourceTDE: &backup.SourceTDEInfo{
			Engine: "cybertec_enterprise",
			KeyRef: "kms-secret://prod/pgee",
		},
		// Manifest invariant: default tablespace must be
		// declared.  Mirrors newFixture's manifest shape.
		Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files:       entries,
		BackupLabel: "START WAL LOCATION: 0/3000028 (file 000000010000000000000003)\n",
	}
	store := backup.NewManifestStore(sp)
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return &fixture{repoURL: repoURL, verifier: verifier, files: files}
}

// TestRestore_SkipsPGVerifybackup_OnTDESource confirms the restore-
// side TDE relaxation: when Manifest.SourceTDE != nil, restore MUST
// NOT run in-process pg_verifybackup (which hashes PLAINTEXT bytes
// against the restored ciphertext bytes — would always mis-fire)
// and MUST emit a verifybackup_skipped_tde notice carrying the
// engine name so the audit trail records the deliberate skip.
//
// Also confirms the negative: a non-TDE manifest still emits
// verifybackup_ok (the historical path).
func TestRestore_SkipsPGVerifybackup_OnTDESource(t *testing.T) {
	t.Run("tde_skip", func(t *testing.T) {
		fx := newTDEFixture(t)
		target := t.TempDir() + "/restored-tde"
		var events []*output.Event
		_, err := restore.Restore(context.Background(), restore.Options{
			RepoURL:    fx.repoURL,
			Deployment: "tde-db",
			BackupID:   "tde-db.full.20260527T120000Z.0001",
			TargetDir:  target,
			Verifier:   fx.verifier,
			OnEvent:    func(e *output.Event) { events = append(events, e) },
		})
		if err != nil {
			t.Fatalf("restore: %v", err)
		}

		var (
			gotSkip bool
			gotOK   bool
			skipEv  *output.Event
		)
		for _, ev := range events {
			switch ev.Op {
			case "verifybackup_skipped_tde":
				gotSkip = true
				skipEv = ev
			case "verifybackup_ok":
				gotOK = true
			}
		}
		if !gotSkip {
			t.Error("expected verifybackup_skipped_tde event; got none")
		}
		if gotOK {
			t.Error("verifybackup_ok fired despite SourceTDE != nil — the skip is the contract")
		}
		// Body must carry the engine so the audit trail tells
		// the operator which TDE engine the backup came from.
		// A regression that dropped the engine string would let
		// the audit record "we skipped verify" but not "for what
		// kind of TDE deployment", which is the actionable bit.
		if skipEv != nil {
			body, ok := skipEv.Body.(map[string]any)
			if !ok {
				t.Errorf("skipEv.Body = %T, want map[string]any", skipEv.Body)
			} else if eng, _ := body["engine"].(string); eng != "cybertec_enterprise" {
				t.Errorf("skipEv.body.engine = %q, want cybertec_enterprise", eng)
			}
		}
	})

	t.Run("non_tde_still_verifies", func(t *testing.T) {
		// Negative: the non-TDE happy path STILL runs the verify
		// gate.  This proves the skip is gated on SourceTDE only
		// and didn't accidentally widen to "skip on any backup".
		fx := newFixture(t)
		target := t.TempDir() + "/restored-vanilla"
		var events []*output.Event
		_, err := restore.Restore(context.Background(), restore.Options{
			RepoURL:    fx.repoURL,
			Deployment: "db1",
			BackupID:   "db1.full.20260428T130000Z.0001",
			TargetDir:  target,
			Verifier:   fx.verifier,
			OnEvent:    func(e *output.Event) { events = append(events, e) },
		})
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
		var (
			gotSkip bool
			gotOK   bool
		)
		for _, ev := range events {
			switch ev.Op {
			case "verifybackup_skipped_tde":
				gotSkip = true
			case "verifybackup_ok", "verifybackup_skipped_no_manifest":
				// Either is acceptable: ok means the gate ran
				// + passed; skipped_no_manifest fires when the
				// fixture didn't stamp PGBackupManifest bytes
				// (which newFixture doesn't — verifybackup needs
				// PG's own manifest to validate against).  The
				// negative we're asserting is that
				// `verifybackup_skipped_tde` did NOT fire.
				gotOK = true
			}
		}
		if gotSkip {
			t.Error("verifybackup_skipped_tde fired on non-TDE backup — the skip widened beyond its contract")
		}
		if !gotOK {
			// The fixture doesn't carry PG's manifest, so the
			// verifybackup logic returns ErrNoManifest and emits
			// verifybackup_skipped_no_manifest.  Either OK event
			// confirms the gate ran rather than being short-
			// circuited by an accidental TDE path.
			t.Error("expected verifybackup_ok or verifybackup_skipped_no_manifest; got neither — the verify gate didn't run")
		}
	})
}

// TestRestore_ForceClearsStaleFiles pins round-3 data-loss #1: a --force
// restore into a non-empty target must DELETE the previous occupant's
// files (the flag's documented "contents will be deleted irrecoverably")
// rather than write the backup over them and leave a stale mix — a
// datadir PG could start as silently corrupt.
func TestRestore_ForceClearsStaleFiles(t *testing.T) {
	fx := newFixture(t)
	target := t.TempDir() + "/restored"
	if err := os.MkdirAll(filepath.Join(target, "old_subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct{ p, b string }{
		{"stale_from_other_cluster", "x"},
		{"old_subdir/nested", "y"},
	} {
		if err := os.WriteFile(filepath.Join(target, f.p), []byte(f.b), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:        fx.repoURL,
		Deployment:     "db1",
		BackupID:       "db1.full.20260428T130000Z.0001",
		TargetDir:      target,
		Verifier:       fx.verifier,
		AllowOverwrite: true,
	}); err != nil {
		t.Fatalf("--force restore: %v", err)
	}

	for _, p := range []string{"stale_from_other_cluster", "old_subdir/nested", "old_subdir"} {
		if _, err := os.Stat(filepath.Join(target, p)); err == nil {
			t.Errorf("stale path %q must be cleared by a --force restore", p)
		}
	}
	// The restored backup's own files must be present.
	if _, err := os.Stat(filepath.Join(target, "PG_VERSION")); err != nil {
		t.Errorf("restored PG_VERSION should be present after --force restore: %v", err)
	}
}

// TestRestore_RefusesNonEmptyTablespaceTarget pins round-3 data-loss
// #2 end-to-end through Restore(): a restore whose --tablespace-mapping
// points at a non-empty external dir is refused (without --force) BEFORE
// any write, so it can't clobber another cluster's tablespace data.
func TestRestore_RefusesNonEmptyTablespaceTarget(t *testing.T) {
	fx := newFixture(t)
	target := t.TempDir() + "/restored"
	tsTarget := t.TempDir()
	if err := os.WriteFile(filepath.Join(tsTarget, "foreign_ts_data"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:         fx.repoURL,
		Deployment:      "db1",
		BackupID:        "db1.full.20260428T130000Z.0001",
		TargetDir:       target,
		Verifier:        fx.verifier,
		TablespaceRemap: restore.TablespaceRemap{{Old: "/srv/old_ts", New: tsTarget}},
	})
	if err == nil {
		t.Fatal("expected refusal: non-empty tablespace target")
	}
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "preflight.tablespace_not_empty" {
		t.Fatalf("code = %v, want preflight.tablespace_not_empty", err)
	}
	// Foreign tablespace data must be untouched (refused before any write).
	if _, err := os.Stat(filepath.Join(tsTarget, "foreign_ts_data")); err != nil {
		t.Errorf("foreign tablespace data must be untouched on refusal: %v", err)
	}
}
