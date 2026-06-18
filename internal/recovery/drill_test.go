package recovery_test

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/recovery"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/verify/sandbox"
)

// drillWorld is a fixture for drill tests. We do NOT use the
// shared setupWorld() from recovery_test.go because the drill
// tests need to inject restoreFn / verifyFn stubs via the
// internal hooks; production callers don't see those.
type drillWorld struct {
	sp       storage.StoragePlugin
	store    *backup.ManifestStore
	signer   *backup.Signer
	verifier *backup.Verifier
	repoURL  string
}

func setupDrillWorld(t *testing.T) *drillWorld {
	t.Helper()
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	return &drillWorld{
		sp:       sp,
		store:    backup.NewManifestStore(sp),
		signer:   signer,
		verifier: verifier,
		repoURL:  repoURL,
	}
}

// commitDrillBackup plants a real (chunked, signed) manifest the
// drill can pick up. Same shape as the readiness-test helper but
// also exposes the manifest for direct access.
//
// The bytes parameter sets BOTH the chunk body size and the
// declared FileEntry.Size, so a real restore (when invoked
// without stubs) can materialise the file without size-mismatch
// errors.
func (w *drillWorld) commitDrillBackup(t *testing.T, deployment string, stoppedAt time.Time, bytes int64) string {
	t.Helper()
	cas := casdefault.New(w.sp)
	if bytes <= 0 {
		bytes = 16
	}
	body := make([]byte, bytes)
	for i := range body {
		body[i] = 'x'
	}
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	id := deployment + ".full." + stoppedAt.Format("20060102T150405.000Z")
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        stoppedAt.Add(-30 * time.Second),
		StoppedAt:        stoppedAt,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{{
			Path: "data/" + id, Size: int64(len(body)), Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: int64(len(body))}},
		}},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	return id
}

// drillOpts builds DrillOptions with the test stubs wired in.
// The actual drill orchestrator is exercised; the heavy
// dependencies (repo open, restore, sandbox.Verify) are stubbed.
func (w *drillWorld) drillOpts(stubRestore stubRestoreFn, stubVerify stubVerifyFn) recovery.DrillOptions {
	return recovery.DrillOptions{
		Verifier: w.verifier,
		// Pass the stubs through the unexported test hooks via
		// the canonical setter helpers below.  We need access
		// to the unexported fields, hence the test-only Setter
		// methods on DrillOptions exposed via internal/recovery's
		// drilltest.go.
	}
}

type stubRestoreFn func(ctx context.Context, opts restore.Options) (*restore.Result, error)
type stubVerifyFn func(ctx context.Context, opts sandbox.Options) (*sandbox.Result, error)

// TestDrill_RequiresArgs: validation guards.
func TestDrill_RequiresArgs(t *testing.T) {
	w := setupDrillWorld(t)
	// Empty repo URL.
	if _, err := recovery.Drill(context.Background(), "", "db1", recovery.DrillOptions{
		Verifier: w.verifier,
	}); err == nil {
		t.Error("empty repoURL must error")
	}
	// Empty deployment.
	if _, err := recovery.Drill(context.Background(), w.repoURL, "", recovery.DrillOptions{
		Verifier: w.verifier,
	}); err == nil {
		t.Error("empty deployment must error")
	}
	// Nil verifier.
	if _, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptions{}); err == nil {
		t.Error("nil verifier must error")
	}
}

// TestDrill_NoBackups: a deployment with no committed backups
// produces a Fail verdict + critical issue, not a hard error.
func TestDrill_NoBackups(t *testing.T) {
	w := setupDrillWorld(t)
	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptions{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	if r.Verdict != recovery.DrillVerdictFail {
		t.Errorf("Verdict = %q, want fail", r.Verdict)
	}
	hasPickIssue := false
	for _, i := range r.Issues {
		if i.Code == "recovery.drill_pick_failed" {
			hasPickIssue = true
		}
	}
	if !hasPickIssue {
		t.Errorf("expected drill_pick_failed issue: %+v", r.Issues)
	}
	// Schema present.
	if r.Schema != recovery.DrillSchema {
		t.Errorf("Schema = %q", r.Schema)
	}
}

// TestRenderDrillMarkdown_Empty: a no-backups report renders
// cleanly without crashing.
func TestRenderDrillMarkdown_Empty(t *testing.T) {
	w := setupDrillWorld(t)
	r, _ := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptions{
		Verifier: w.verifier,
	})
	var sb strings.Builder
	if err := recovery.RenderDrillMarkdown(&sb, r); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"# pg_hardstorage recovery drill",
		"## Verdict",
		"## Phases",
		"## RTO actual vs target",
		"FAIL",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Markdown missing %q:\n%s", want, out)
		}
	}
}

// TestRenderDrillMarkdown_Nil: rendering nil errors cleanly.
func TestRenderDrillMarkdown_Nil(t *testing.T) {
	var sb strings.Builder
	if err := recovery.RenderDrillMarkdown(&sb, nil); err == nil {
		t.Error("expected error for nil report")
	}
}

// TestDrill_VerdictGlyphCoverage: every DrillVerdict value
// produces a renderable Markdown without hitting a default branch.
func TestDrill_VerdictGlyphCoverage(t *testing.T) {
	for _, v := range []recovery.DrillVerdict{
		recovery.DrillVerdictPass,
		recovery.DrillVerdictPartial,
		recovery.DrillVerdictFail,
	} {
		r := &recovery.DrillReport{
			Schema:     recovery.DrillSchema,
			Deployment: "db1",
			Verdict:    v,
		}
		var sb strings.Builder
		if err := recovery.RenderDrillMarkdown(&sb, r); err != nil {
			t.Fatalf("verdict %q: %v", v, err)
		}
		if !strings.Contains(sb.String(), strings.ToUpper(string(v))) {
			t.Errorf("verdict %q didn't appear in output", v)
		}
	}
}

// TestDrill_PicksLatestBackup: when BackupID is "latest", the
// drill picks the freshest StoppedAt manifest.
func TestDrill_PicksLatestBackup(t *testing.T) {
	w := setupDrillWorld(t)
	now := time.Now().UTC()
	w.commitDrillBackup(t, "db1", now.Add(-2*time.Hour), 100)
	expectLatest := w.commitDrillBackup(t, "db1", now.Add(-1*time.Hour), 200)

	// We can't run a real drill here without Docker; this test
	// just confirms the pick phase resolves "latest" correctly
	// by inspecting the BackupID field after the (failing)
	// restore phase.  The restore phase will fail because we
	// don't have a real CAS for the inflated chunks; the test
	// asserts BackupID is set to expectLatest before the
	// failure.
	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptions{
		Verifier:           w.verifier,
		BackupID:           "latest",
		SkipVerifyEntirely: true,
	})
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	if r.BackupID != expectLatest {
		t.Errorf("BackupID = %q, want %q", r.BackupID, expectLatest)
	}
}

// TestDrill_ExplicitBackupID: explicit BackupID argument is
// honoured over "latest".
func TestDrill_ExplicitBackupID(t *testing.T) {
	w := setupDrillWorld(t)
	now := time.Now().UTC()
	older := w.commitDrillBackup(t, "db1", now.Add(-2*time.Hour), 100)
	w.commitDrillBackup(t, "db1", now.Add(-1*time.Hour), 200)

	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptions{
		Verifier:           w.verifier,
		BackupID:           older,
		SkipVerifyEntirely: true,
	})
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	if r.BackupID != older {
		t.Errorf("BackupID = %q, want %q (explicit)", r.BackupID, older)
	}
}

// TestDrill_ExplicitBackupID_Missing: an explicit BackupID that
// doesn't resolve fails cleanly.
func TestDrill_ExplicitBackupID_Missing(t *testing.T) {
	w := setupDrillWorld(t)
	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptions{
		Verifier: w.verifier,
		BackupID: "db1.full.NEVER",
	})
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	if r.Verdict != recovery.DrillVerdictFail {
		t.Errorf("Verdict = %q, want fail", r.Verdict)
	}
}

// TestDrill_TargetDirCleanedUp: by default the drill removes its
// temp dir.  We verify by capturing the path post-failure (the
// teardown phase still runs) and asserting it's gone.
func TestDrill_TargetDirCleanedUp(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackup(t, "db1", time.Now().UTC().Add(-1*time.Hour), 100)

	tempBase := t.TempDir()
	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptions{
		Verifier:           w.verifier,
		TempBaseDir:        tempBase,
		SkipVerifyEntirely: true,
	})
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	// After teardown, TargetDir is cleared and the dir should
	// not exist.  The dir path was reported in the prepare phase
	// note — let's check the parent base is empty.
	entries, err := os.ReadDir(tempBase)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "pg_hardstorage-drill-db1-") {
			t.Errorf("drill dir not cleaned up: %s", e.Name())
		}
	}
	// TargetDir field is cleared post-teardown.
	if r.TargetDir != "" {
		t.Errorf("TargetDir = %q, want empty post-teardown", r.TargetDir)
	}
}

// TestDrill_KeepTargetDir: --keep leaves the dir in place + the
// path is reported.
func TestDrill_KeepTargetDir(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackup(t, "db1", time.Now().UTC().Add(-1*time.Hour), 100)

	tempBase := t.TempDir()
	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptions{
		Verifier:           w.verifier,
		TempBaseDir:        tempBase,
		KeepTargetDir:      true,
		SkipVerifyEntirely: true,
	})
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	if r.TargetDir == "" {
		t.Fatal("TargetDir should be set with KeepTargetDir")
	}
	// Dir should exist (unless restore failed before
	// prepareDrillDir; but prepare runs after pick which we know
	// succeeded).
	if _, err := os.Stat(r.TargetDir); err != nil {
		t.Errorf("target dir gone: %v", err)
	}
	// Manual cleanup for the test.
	_ = os.RemoveAll(r.TargetDir)
}

// TestDrill_SkipVerifyEntirely_ReturnsPartial: the strongest
// opt-out skips verify and returns a Partial verdict.
func TestDrill_SkipVerifyEntirely_ReturnsPartial(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackup(t, "db1", time.Now().UTC().Add(-1*time.Hour), 100)

	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptions{
		Verifier:           w.verifier,
		SkipVerifyEntirely: true,
	})
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	if r.Verdict != recovery.DrillVerdictPartial {
		t.Errorf("Verdict = %q, want partial", r.Verdict)
	}
	hasNotice := false
	for _, i := range r.Issues {
		if i.Code == "recovery.drill_verify_skipped_explicitly" {
			hasNotice = true
		}
	}
	if !hasNotice {
		t.Errorf("expected drill_verify_skipped_explicitly notice: %+v", r.Issues)
	}
}

// TestDrill_PhasesRecorded: every successful phase is captured
// with timing.
func TestDrill_PhasesRecorded(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackup(t, "db1", time.Now().UTC().Add(-1*time.Hour), 100)

	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptions{
		Verifier:           w.verifier,
		SkipVerifyEntirely: true,
	})
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	wantPhases := []string{"pick", "prepare", "restore"}
	for _, name := range wantPhases {
		p := r.PhaseByName(name)
		if p.Name != name {
			t.Errorf("phase %q missing", name)
		}
	}
	// Teardown runs at the end (deferred); recorded after we
	// returned but appended via the deferred closure.
	td := r.PhaseByName("teardown")
	if td.Name != "teardown" {
		t.Errorf("teardown phase missing: %+v", r.Phases)
	}
}

// TestDrill_RTOEstimate_Surfaced: --rto-seconds carries through
// to the report.
func TestDrill_RTOEstimate_Surfaced(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackup(t, "db1", time.Now().UTC().Add(-1*time.Hour), 100)

	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptions{
		Verifier:           w.verifier,
		RTOEstimateSeconds: 300,
		SkipVerifyEntirely: true,
	})
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	if r.RTOEstimateSeconds != 300 {
		t.Errorf("RTOEstimateSeconds = %d, want 300", r.RTOEstimateSeconds)
	}
}

// TestDrill_RestoreErrorsCleanly: stub a restore that errors;
// drill returns a Fail verdict + structured issue.
func TestDrill_RestoreErrorsCleanly(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackup(t, "db1", time.Now().UTC().Add(-1*time.Hour), 100)

	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptionsWithStubs(
		recovery.DrillOptions{
			Verifier:           w.verifier,
			SkipVerifyEntirely: true,
		},
		func(ctx context.Context, opts restore.Options) (*restore.Result, error) {
			return nil, errors.New("simulated restore failure")
		},
		nil,
	))
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	if r.Verdict != recovery.DrillVerdictFail {
		t.Errorf("Verdict = %q, want fail", r.Verdict)
	}
	hasIssue := false
	for _, i := range r.Issues {
		if i.Code == "recovery.drill_restore_failed" {
			hasIssue = true
		}
	}
	if !hasIssue {
		t.Errorf("expected drill_restore_failed: %+v", r.Issues)
	}
}

// TestDrill_VerifyPasses: stub a passing verify; drill returns a
// Pass verdict + verify section in the report.
func TestDrill_VerifyPasses(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackup(t, "db1", time.Now().UTC().Add(-1*time.Hour), 100)

	dir := t.TempDir()
	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptionsWithStubs(
		recovery.DrillOptions{
			Verifier:    w.verifier,
			TempBaseDir: dir,
		},
		// Passing restore stub.
		func(ctx context.Context, opts restore.Options) (*restore.Result, error) {
			return &restore.Result{
				BackupID:      opts.BackupID,
				Deployment:    opts.Deployment,
				TargetDir:     opts.TargetDir,
				FileCount:     1,
				BytesWritten:  100,
				ChunksFetched: 1,
				StartedAt:     time.Now().UTC(),
				StoppedAt:     time.Now().UTC().Add(1 * time.Second),
				Duration:      1 * time.Second,
			}, nil
		},
		// Passing verify stub.
		func(ctx context.Context, opts sandbox.Options) (*sandbox.Result, error) {
			return &sandbox.Result{
				Schema:    sandbox.SchemaResult,
				PGMajor:   opts.PGMajor,
				Image:     "postgres:" + opts.PGMajor,
				Passed:    true,
				Tool:      "pg_verifybackup",
				StartedAt: time.Now().UTC(),
				StoppedAt: time.Now().UTC().Add(2 * time.Second),
				Duration:  2 * time.Second,
			}, nil
		},
	))
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	if r.Verdict != recovery.DrillVerdictPass {
		t.Errorf("Verdict = %q, want pass", r.Verdict)
	}
	if r.Verify == nil || !r.Verify.Passed {
		t.Errorf("Verify = %+v", r.Verify)
	}
	if r.Restore == nil || r.Restore.FileCount != 1 {
		t.Errorf("Restore = %+v", r.Restore)
	}
}

// TestDrill_VerifyFails: a failing pg_verifybackup → Fail verdict.
func TestDrill_VerifyFails(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackup(t, "db1", time.Now().UTC().Add(-1*time.Hour), 100)

	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptionsWithStubs(
		recovery.DrillOptions{Verifier: w.verifier, TempBaseDir: t.TempDir()},
		func(ctx context.Context, opts restore.Options) (*restore.Result, error) {
			return &restore.Result{BackupID: opts.BackupID, Deployment: opts.Deployment, TargetDir: opts.TargetDir}, nil
		},
		func(ctx context.Context, opts sandbox.Options) (*sandbox.Result, error) {
			return &sandbox.Result{
				Schema:  sandbox.SchemaResult,
				PGMajor: opts.PGMajor,
				Image:   "postgres:" + opts.PGMajor,
				Passed:  false,
				Stderr:  "pg_verifybackup: error: file \"global/pg_control\" has size 0 in backup",
			}, nil
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != recovery.DrillVerdictFail {
		t.Errorf("Verdict = %q, want fail", r.Verdict)
	}
	hasIssue := false
	for _, i := range r.Issues {
		if i.Code == "recovery.drill_verify_failed" {
			hasIssue = true
		}
	}
	if !hasIssue {
		t.Errorf("expected drill_verify_failed: %+v", r.Issues)
	}
}

// TestDrill_VerifySkipped_AsFail: a skipped verify without
// AllowSkipVerify yields a Fail.
func TestDrill_VerifySkipped_AsFail(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackup(t, "db1", time.Now().UTC().Add(-1*time.Hour), 100)

	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptionsWithStubs(
		recovery.DrillOptions{Verifier: w.verifier, TempBaseDir: t.TempDir()},
		func(ctx context.Context, opts restore.Options) (*restore.Result, error) {
			return &restore.Result{BackupID: opts.BackupID, Deployment: opts.Deployment, TargetDir: opts.TargetDir}, nil
		},
		func(ctx context.Context, opts sandbox.Options) (*sandbox.Result, error) {
			return &sandbox.Result{
				Schema:     sandbox.SchemaResult,
				PGMajor:    opts.PGMajor,
				Image:      "postgres:" + opts.PGMajor,
				Skipped:    true,
				SkipReason: "no backup_manifest in PGDATA",
			}, nil
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != recovery.DrillVerdictFail {
		t.Errorf("Verdict = %q, want fail (default skip = fail)", r.Verdict)
	}
}

// TestDrill_VerifySkipped_AsPartial: AllowSkipVerify converts a
// skip to Partial.
func TestDrill_VerifySkipped_AsPartial(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackup(t, "db1", time.Now().UTC().Add(-1*time.Hour), 100)

	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptionsWithStubs(
		recovery.DrillOptions{
			Verifier:        w.verifier,
			TempBaseDir:     t.TempDir(),
			AllowSkipVerify: true,
		},
		func(ctx context.Context, opts restore.Options) (*restore.Result, error) {
			return &restore.Result{BackupID: opts.BackupID, Deployment: opts.Deployment, TargetDir: opts.TargetDir}, nil
		},
		func(ctx context.Context, opts sandbox.Options) (*sandbox.Result, error) {
			return &sandbox.Result{
				Schema:     sandbox.SchemaResult,
				PGMajor:    opts.PGMajor,
				Image:      "postgres:" + opts.PGMajor,
				Skipped:    true,
				SkipReason: "no backup_manifest in PGDATA",
			}, nil
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != recovery.DrillVerdictPartial {
		t.Errorf("Verdict = %q, want partial", r.Verdict)
	}
}

// TestDrill_VerifyErrors_StructuredIssue: stub a sandbox error
// (e.g. Docker unavailable); drill records a critical issue.
func TestDrill_VerifyErrors_StructuredIssue(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackup(t, "db1", time.Now().UTC().Add(-1*time.Hour), 100)

	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptionsWithStubs(
		recovery.DrillOptions{Verifier: w.verifier, TempBaseDir: t.TempDir()},
		func(ctx context.Context, opts restore.Options) (*restore.Result, error) {
			return &restore.Result{BackupID: opts.BackupID, Deployment: opts.Deployment, TargetDir: opts.TargetDir}, nil
		},
		func(ctx context.Context, opts sandbox.Options) (*sandbox.Result, error) {
			return nil, errors.New("docker daemon not running")
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != recovery.DrillVerdictFail {
		t.Errorf("Verdict = %q, want fail (verify errored)", r.Verdict)
	}
	hasIssue := false
	for _, i := range r.Issues {
		if i.Code == "recovery.drill_verify_errored" {
			hasIssue = true
		}
	}
	if !hasIssue {
		t.Errorf("expected drill_verify_errored: %+v", r.Issues)
	}
}

// TestDrill_RenderMarkdown_Pass: rendering a pass report.
func TestDrill_RenderMarkdown_Pass(t *testing.T) {
	r := &recovery.DrillReport{
		Schema:           recovery.DrillSchema,
		Deployment:       "db1",
		BackupID:         "db1.full.X",
		Verdict:          recovery.DrillVerdictPass,
		RTOActualSeconds: 47,
		Phases: []recovery.DrillPhase{
			{Name: "pick", OK: true, Note: "picked db1.full.X"},
			{Name: "prepare", OK: true, Note: "target dir /tmp/x"},
			{Name: "restore", OK: true, Note: "restored 1 file"},
			{Name: "verify", OK: true, Note: "pg_verifybackup passed"},
		},
		Restore: &restore.Result{FileCount: 1, BytesWritten: 100, ChunksFetched: 1},
		Verify: &sandbox.Result{
			Schema:  sandbox.SchemaResult,
			Tool:    "pg_verifybackup",
			Image:   "postgres:17",
			PGMajor: "17",
			Passed:  true,
		},
		GeneratedAt: time.Now().UTC(),
	}
	var sb strings.Builder
	if err := recovery.RenderDrillMarkdown(&sb, r); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"# pg_hardstorage recovery drill",
		"## Verdict",
		"PASS",
		"## Phases",
		"## RTO actual vs target",
		"## Restore detail",
		"## Verify detail",
		"`pg_verifybackup`",
		"✓ passed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Markdown missing %q in:\n%s", want, out)
		}
	}
}

// TestDrill_AnyPhaseFailed: helper reports correctly.
func TestDrill_AnyPhaseFailed(t *testing.T) {
	r := &recovery.DrillReport{
		Phases: []recovery.DrillPhase{
			{Name: "pick", OK: true},
			{Name: "prepare", OK: true},
			{Name: "restore", OK: false},
			{Name: "teardown", OK: false},
		},
	}
	if !r.AnyPhaseFailed() {
		t.Error("AnyPhaseFailed = false; want true (restore failed)")
	}
	r2 := &recovery.DrillReport{
		Phases: []recovery.DrillPhase{
			{Name: "pick", OK: true},
			{Name: "prepare", OK: true},
			{Name: "teardown", OK: false}, // teardown excluded
		},
	}
	if r2.AnyPhaseFailed() {
		t.Error("AnyPhaseFailed = true; teardown alone shouldn't count")
	}
}

// TestDrill_AbsolutePathOnly: cleanup helper refuses non-absolute
// paths for safety (defence against path-traversal in tests).
func TestDrill_AbsolutePathOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := recovery.DrillTargetCleanupForTest(dir); err != nil {
		t.Errorf("absolute cleanup should succeed: %v", err)
	}
	if err := recovery.DrillTargetCleanupForTest("not-absolute"); err == nil {
		t.Error("non-absolute path should error")
	}
	if err := recovery.DrillTargetCleanupForTest(""); err != nil {
		t.Errorf("empty path is no-op: %v", err)
	}
}
