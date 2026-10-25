package cli_test

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// readWorld is a self-contained test-fixture: an initialised repo and
// a keypair on disk that BOTH the test and the CLI will agree on.
//
// Mechanism: bootstrap HOME and clear PG_HARDSTORAGE_* env so the CLI's
// `paths.Resolve(paths.DefaultOptions())` resolves to the test's keyring
// dir. Pre-populate that dir via keystore.LoadOrGenerate so the test
// holds the matching signer.
type readWorld struct {
	repoURL  string
	signer   *backup.Signer
	verifier *backup.Verifier
	sp       storage.StoragePlugin
	store    *backup.ManifestStore
	// configDir is the resolved config directory under the test's
	// HOME — same path loadEditableConfig uses. Tests that need to
	// plant a pg_hardstorage.yaml visible to the CLI write into it
	// directly rather than calling configDir(t) (which would set a
	// different HOME and decouple the keyring).
	configDir string
}

func (w *readWorld) cleanup() { _ = w.sp.Close() }

func newReadWorld(t *testing.T) *readWorld {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_RUNTIME_DIR", "")

	// Resolve paths the way the CLI will.
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	signer, verifier, err := keystore.LoadOrGenerate(p.Keyring.Value)
	if err != nil {
		t.Fatal(err)
	}

	// Init a repo at a temp dir.
	repoRoot := t.TempDir()
	repoURL := "file://" + repoRoot
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: mustParseURL(t, repoURL)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return &readWorld{
		repoURL:   repoURL,
		signer:    signer,
		verifier:  verifier,
		sp:        sp,
		store:     backup.NewManifestStore(sp),
		configDir: p.Config.Value,
	}
}

// commitManifest commits a manifest belonging to deployment with the
// given index (used to produce a unique BackupID and a deterministic
// StoppedAt).
func (w *readWorld) commitManifest(t *testing.T, deployment string, idx int) {
	t.Helper()
	ts := time.Date(2026, 4, 28, 14, idx, 0, 0, time.UTC)
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         deployment + ".full." + ts.Format("20060102T150405Z") + ".000" + string(rune('1'+idx)),
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        ts,
		StoppedAt:        ts.Add(30 * time.Second),
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces: []backup.Tablespace{
			{OID: 1663, Location: "pg_default"},
		},
		Files: []backup.FileEntry{
			{Path: "PG_VERSION", Size: 3, Mode: 0o600,
				Chunks: []backup.ChunkRef{{Hash: repo.HashOf([]byte("17\n")), Offset: 0, Len: 3}}},
		},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit %s/%d: %v", deployment, idx, err)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func runCLI(t *testing.T, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	root := cli.NewRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(args)
	exit = cli.Run(root)
	return out.String(), errb.String(), exit
}

func bodyOf(t *testing.T, raw string, into any) {
	t.Helper()
	var res output.Result
	if err := stdjson.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("unwrap Result: %v\n%s", err, raw)
	}
	if res.IsError() {
		t.Fatalf("unexpected error result: %+v", res.Error)
	}
	bodyBytes, err := stdjson.Marshal(res.Result)
	if err != nil {
		t.Fatal(err)
	}
	if err := stdjson.Unmarshal(bodyBytes, into); err != nil {
		t.Fatalf("decode body: %v\n%s", err, bodyBytes)
	}
}

// ----- list -----

func TestList_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t) // sets HOME
	_, errb, exit := runCLI(t, "list", "db1", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag: %s", errb)
	}
}

func TestList_EmptyDeployment(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "list", "db1", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view struct {
		Deployment string `json:"deployment"`
		Count      int    `json:"count"`
	}
	bodyOf(t, stdout, &view)
	if view.Count != 0 {
		t.Errorf("count = %d, want 0", view.Count)
	}
}

func TestList_DescOrder(t *testing.T) {
	w := newReadWorld(t)
	for i := 0; i < 4; i++ {
		w.commitManifest(t, "db1", i)
	}
	stdout, _, exit := runCLI(t, "list", "db1", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view struct {
		Count   int `json:"count"`
		Backups []struct {
			BackupID  string    `json:"backup_id"`
			StoppedAt time.Time `json:"stopped_at"`
		} `json:"backups"`
	}
	bodyOf(t, stdout, &view)
	if view.Count != 4 {
		t.Fatalf("count = %d, want 4", view.Count)
	}
	for i := 1; i < len(view.Backups); i++ {
		if view.Backups[i].StoppedAt.After(view.Backups[i-1].StoppedAt) {
			t.Errorf("not descending: %v vs %v",
				view.Backups[i-1].StoppedAt, view.Backups[i].StoppedAt)
		}
	}
}

// ----- show -----

func TestShow_NotFound(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)
	_, errb, exit := runCLI(t, "show", "db1", "no-such-id", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitNotFound) {
		t.Errorf("exit = %d, want ExitNotFound(%d)", exit, output.ExitNotFound)
	}
	if !strings.Contains(errb, "notfound.backup") {
		t.Errorf("error should carry notfound.backup code: %s", errb)
	}
}

func TestShow_HappyPath(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)

	// First, list to grab the actual ID.
	stdout, _, _ := runCLI(t, "list", "db1", "--repo", w.repoURL, "-o", "json")
	var listView struct {
		Backups []struct {
			BackupID string `json:"backup_id"`
		} `json:"backups"`
	}
	bodyOf(t, stdout, &listView)
	if len(listView.Backups) != 1 {
		t.Fatalf("expected 1 backup, got %d", len(listView.Backups))
	}
	id := listView.Backups[0].BackupID

	// Now show it.
	stdout, _, exit := runCLI(t, "show", "db1", id, "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view struct {
		BackupID                        string `json:"backup_id"`
		Deployment                      string `json:"deployment"`
		PGVersion                       int    `json:"pg_version"`
		FileCount                       int    `json:"file_count"`
		AttestationPublicKeyFingerprint string `json:"attestation_public_key_fingerprint"`
	}
	bodyOf(t, stdout, &view)
	// `file_count` isn't a field we set, but `files` array is on the
	// embedded Manifest. Verify the shape via the underlying manifest
	// fields we expect.
	if view.BackupID != id {
		t.Errorf("BackupID = %q, want %q", view.BackupID, id)
	}
	if view.Deployment != "db1" {
		t.Errorf("Deployment = %q", view.Deployment)
	}
	if view.PGVersion != 17 {
		t.Errorf("PGVersion = %d", view.PGVersion)
	}
	if view.AttestationPublicKeyFingerprint == "" {
		t.Error("AttestationPublicKeyFingerprint should be populated")
	}
}

// ----- status -----

func TestStatus_NoDeployments(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "status", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view struct {
		Deployments []any `json:"deployments"`
	}
	bodyOf(t, stdout, &view)
	if len(view.Deployments) != 0 {
		t.Errorf("expected 0 deployments; got %d", len(view.Deployments))
	}
}

func TestStatus_SingleDeployment(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)
	w.commitManifest(t, "db1", 1)

	stdout, _, exit := runCLI(t, "status", "db1", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view struct {
		Deployments []struct {
			Deployment     string `json:"deployment"`
			BackupCount    int    `json:"backup_count"`
			LatestBackupID string `json:"latest_backup_id"`
		} `json:"deployments"`
	}
	bodyOf(t, stdout, &view)
	if len(view.Deployments) != 1 {
		t.Fatalf("expected 1 deployment; got %d", len(view.Deployments))
	}
	if view.Deployments[0].BackupCount != 2 {
		t.Errorf("BackupCount = %d, want 2", view.Deployments[0].BackupCount)
	}
}

func TestStatus_AllDeployments(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)
	w.commitManifest(t, "db2", 0)
	w.commitManifest(t, "db2", 1)
	w.commitManifest(t, "analytics", 0)

	stdout, _, exit := runCLI(t, "status", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view struct {
		Deployments []struct {
			Deployment  string `json:"deployment"`
			BackupCount int    `json:"backup_count"`
		} `json:"deployments"`
	}
	bodyOf(t, stdout, &view)
	if len(view.Deployments) != 3 {
		t.Fatalf("expected 3 deployments; got %d", len(view.Deployments))
	}
	counts := map[string]int{}
	for _, d := range view.Deployments {
		counts[d.Deployment] = d.BackupCount
	}
	if counts["db1"] != 1 || counts["db2"] != 2 || counts["analytics"] != 1 {
		t.Errorf("counts wrong: %v", counts)
	}
}

// A manifest that exists but cannot be parsed is damage, reported in the
// same namespace as `verify`; it used to surface as code "internal".
func TestShow_CorruptManifestIsNotAnInternalError(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)
	var path string
	_ = filepath.Walk(strings.TrimPrefix(w.repoURL, "file://"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.HasSuffix(p, "/manifest.json") {
			path = p
		}
		return nil
	})
	if path == "" {
		t.Fatal("fixture: primary manifest not found")
	}
	fi, _ := os.Stat(path)
	if err := os.Truncate(path, fi.Size()/2); err != nil {
		t.Fatal(err)
	}
	// Drop the replica so there is nothing to fall back to.
	_ = os.RemoveAll(filepath.Join(strings.TrimPrefix(w.repoURL, "file://"), "manifests", "_replicas"))

	id := filepath.Base(filepath.Dir(path))
	_, errb, exit := runCLI(t, "show", "db1", id, "--repo", w.repoURL, "-o", "json")
	if strings.Contains(errb, `"internal"`) {
		t.Errorf("corrupt manifest reported as an internal error: %s", errb)
	}
	if !strings.Contains(errb, "verify.manifest_signature") {
		t.Errorf("error should carry verify.manifest_signature: %s", errb)
	}
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("exit = %d, want ExitVerifyFailed(%d)", exit, output.ExitVerifyFailed)
	}
}

// `partial inspect` reads manifests without verifying them; with no
// database connection it still maps the requested tables to nothing and
// must say so rather than fail.
func TestPartialInspect_ReadsTheNewestBackup(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)
	stdout, errb, exit := runCLI(t, "partial", "inspect", "db1", "--repo", w.repoURL,
		"--tables", "public.t1", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", exit, stdout, errb)
	}
	var view struct {
		BackupID string `json:"backup_id"`
	}
	bodyOf(t, stdout, &view)
	if !strings.HasPrefix(view.BackupID, "db1.full.") {
		t.Fatalf("inspected backup %q, want the committed db1 backup", view.BackupID)
	}
}

// `verify latest` must say so when it passed over a manifest it could not
// read: silently verifying an older backup would hide that the newest one
// is damaged.
func TestVerifyLatest_WarnsWhenItSkippedAnUnreadableManifest(t *testing.T) {
	w := newReadWorld(t)
	w.commitManifest(t, "db1", 0)
	w.commitManifest(t, "db1", 1)
	var newest string
	_ = filepath.Walk(strings.TrimPrefix(w.repoURL, "file://"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.HasSuffix(p, "/manifest.json") && p > newest {
			newest = p
		}
		return nil
	})
	if err := os.WriteFile(newest, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(filepath.Join(strings.TrimPrefix(w.repoURL, "file://"), "manifests", "_replicas"))
	// The note goes to the process's stderr, not the command's writer.
	r, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = wr
	_, _, _ = runCLI(t, "verify", "db1", "latest", "--repo", w.repoURL, "-o", "json")
	os.Stderr = saved
	_ = wr.Close()
	captured, _ := io.ReadAll(r)
	if !strings.Contains(string(captured), "skipped 1 manifest") {
		t.Fatalf("no skip warning on stderr:\n%s", captured)
	}
}
