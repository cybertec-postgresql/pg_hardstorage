// drill.go — recovery-drill runner: restore-to-sandbox + pg_verifybackup with DrillReport verdict.
package recovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/verify/sandbox"
)

// DrillSchema is the on-disk version tag for DrillReport bodies.
const DrillSchema = "pg_hardstorage.recovery.drill.v1"

// DrillVerdict is the single-word outcome of a drill run.
type DrillVerdict string

const (
	// DrillVerdictPass — the restore completed and pg_verifybackup
	// passed.  This is the "drill succeeded" verdict.
	DrillVerdictPass DrillVerdict = "pass"
	// DrillVerdictFail — at least one phase failed: restore
	// errored, OR pg_verifybackup reported a problem.
	DrillVerdictFail DrillVerdict = "fail"
	// DrillVerdictPartial — restore succeeded but verify was
	// skipped (pg_verifybackup is unavailable, manifest is
	// missing the on-disk backup_manifest, etc.).  Operators
	// running with `--allow-skip-verify` accept this verdict.
	DrillVerdictPartial DrillVerdict = "partial"
)

// DrillReport is the structured outcome of one drill run.  Stable
// per the v1 schema commitment.
type DrillReport struct {
	Schema      string    `json:"schema"`
	GeneratedAt time.Time `json:"generated_at"`
	StoppedAt   time.Time `json:"stopped_at"`
	DurationMS  int64     `json:"duration_ms"`

	URL        string `json:"url"`
	Deployment string `json:"deployment"`
	BackupID   string `json:"backup_id"`

	// TargetDir is the temporary directory used for the restore.
	// Empty when AutoTearDown is true (the dir was removed before
	// returning).  Non-empty when --keep was passed; operators
	// can inspect the data dir post-drill.
	TargetDir string `json:"target_dir,omitempty"`

	// Phases tracks per-phase status.  Each phase has a Started /
	// Stopped / Duration and either an Error or a structured
	// payload.  Renderers iterate phases in order.
	Phases []DrillPhase `json:"phases"`

	// Restore captures the inner Result.  Empty when the restore
	// phase didn't run (e.g. backup-pick failed first).
	Restore *restore.Result `json:"restore,omitempty"`

	// Verify captures the sandbox.Result.  Empty when the verify
	// phase didn't run or was skipped.
	Verify *sandbox.Result `json:"verify,omitempty"`

	// RTOActualSeconds is the wallclock time from drill start to
	// successful restore (excluding verify).  Operators compare
	// this to their RTO target.  Zero when the restore failed.
	RTOActualSeconds int64 `json:"rto_actual_seconds"`

	// RTOEstimateSeconds carries the prior estimate (from
	// readiness or the manifest's logical bytes / throughput
	// model) so the report shows actual-vs-estimate side by side.
	// Zero when no estimate was supplied.
	RTOEstimateSeconds int64 `json:"rto_estimate_seconds,omitempty"`

	// Verdict is the overall outcome.
	Verdict DrillVerdict `json:"verdict"`

	// Issues are operator-facing findings.  Each carries a
	// severity / code / message / suggestion in the same shape
	// as readiness Issues.
	Issues []ReadinessIssue `json:"issues,omitempty"`
}

// DrillPhase is one phase of the drill (pick, restore, verify,
// teardown).
type DrillPhase struct {
	Name       string    `json:"name"`
	StartedAt  time.Time `json:"started_at"`
	StoppedAt  time.Time `json:"stopped_at"`
	DurationMS int64     `json:"duration_ms"`
	OK         bool      `json:"ok"`
	Error      string    `json:"error,omitempty"`
	Note       string    `json:"note,omitempty"`
}

// DrillOptions configures one Drill() run.
type DrillOptions struct {
	// Verifier validates the source manifest's signature.
	// Required.
	Verifier *backup.Verifier

	// BackupID is the manifest to drill against.  Empty or
	// "latest" picks the freshest backup.
	BackupID string

	// PGMajor controls the sandbox image (e.g. "17").  Default:
	// derived from the manifest's PGVersion.
	PGMajor string

	// SandboxImage overrides `postgres:<major>` for air-gapped
	// environments.
	SandboxImage string

	// TempBaseDir is the parent directory under which the drill
	// creates its temporary target dir.  Default os.TempDir().
	TempBaseDir string

	// KeepTargetDir, when true, leaves the temporary target dir
	// in place after the drill.  The DrillReport carries the
	// path so the operator can inspect.  Default: tear down.
	KeepTargetDir bool

	// AllowSkipVerify, when true, accepts a Partial verdict
	// without converting it to a Fail.  Default: a skipped
	// verify is a Fail (operators dropped a missing backup_manifest
	// in the source manifest want to know).
	AllowSkipVerify bool

	// SkipVerifyEntirely is the strongest opt-out: don't even
	// try to spin up the sandbox.  Used when the operator wants
	// a pure "did the restore work?" drill without Docker.
	SkipVerifyEntirely bool

	// RTOEstimateSeconds carries an external RTO target /
	// estimate to show alongside RTOActualSeconds in the report.
	// Zero = no estimate.
	RTOEstimateSeconds int64

	// KEKResolver resolves the manifest's KEKRef → KEK bytes.
	// Required when the chosen manifest is encrypted.
	KEKResolver func(ref string) ([encryption.KeyLen]byte, error)

	// DEKUnwrapper unwraps a cloud-KMS-wrapped DEK server-side (issue #102);
	// required to drill a backup wrapped with a cloud KMS KEK. Forwarded to
	// restore.Options.UnwrapDEK.
	DEKUnwrapper func(ctx context.Context, kekRef string, wrapped []byte) ([]byte, error)

	// Now overrides time.Now() for deterministic tests.
	Now time.Time

	// OnEvent receives restore progress events.  Optional.
	OnEvent func(*output.Event)

	// SkipHistory, when true, suppresses the post-drill
	// auto-persist of a slim DrillHistoryEntry into
	// recovery/drills/.  Default behaviour: every drill run
	// records a history entry so trend analysis is observable
	// fleet-wide.  Tests + ad-hoc one-off drills pass true to
	// keep the repo tidy.
	SkipHistory bool

	// Operator records who initiated the drill in the history
	// entry.  Free-form; cron-driven scheduled runs typically
	// pass "scheduler:<task-id>".  Empty is fine; the field
	// is omitted from the persisted record.
	Operator string

	// repoOpener is the (test-injectable) open-repo callback.
	// Production callers leave it nil; tests pass a stub for
	// the no-Docker drill paths.  Unexported because operators
	// shouldn't override the canonical repo.Open.
	repoOpener func(ctx context.Context, url string) (*repo.Metadata, storage.StoragePlugin, error)

	// restoreFn is the (test-injectable) restore callback.  When
	// nil, Drill calls restore.Restore directly.  Set in tests
	// to bypass the actual chunk-fetch path and exercise the
	// orchestration logic without storage round-trips.
	restoreFn func(ctx context.Context, opts restore.Options) (*restore.Result, error)

	// verifyFn is the (test-injectable) sandbox-verify callback.
	// When nil, Drill calls sandbox.Verify directly.  Set in
	// tests to short-circuit the Docker dependency.
	verifyFn func(ctx context.Context, opts sandbox.Options) (*sandbox.Result, error)
}

// Drill runs one DR drill against the named deployment.  Composes
// pickBackup + restore + sandbox.Verify; produces a structured
// DrillReport with per-phase timing + RTO actual.
//
// Read-only against the source repo.  Writes to a temporary
// target dir which is torn down on return (unless KeepTargetDir).
func Drill(ctx context.Context, repoURL string, deployment string, opts DrillOptions) (*DrillReport, error) {
	if repoURL == "" {
		return nil, errors.New("recovery: drill: empty repoURL")
	}
	if deployment == "" {
		return nil, errors.New("recovery: drill: empty deployment")
	}
	if opts.Verifier == nil {
		return nil, errors.New("recovery: drill: Verifier is required")
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	started := time.Now().UTC()
	report := &DrillReport{
		Schema:             DrillSchema,
		GeneratedAt:        now,
		URL:                repoURL,
		Deployment:         deployment,
		Phases:             []DrillPhase{},
		RTOEstimateSeconds: opts.RTOEstimateSeconds,
		Verdict:            DrillVerdictFail, // overwritten on success
	}
	finish := func() {
		report.StoppedAt = time.Now().UTC()
		report.DurationMS = report.StoppedAt.Sub(started).Milliseconds()
	}

	openFn := opts.repoOpener
	if openFn == nil {
		openFn = repo.Open
	}

	_, sp, err := openFn(ctx, repoURL)
	if err != nil {
		report.Issues = append(report.Issues, ReadinessIssue{
			Severity:   SeverityCritical,
			Code:       "recovery.drill_open_repo_failed",
			Message:    fmt.Sprintf("open repo %q: %v", repoURL, err),
			Suggestion: "verify the repository URL is reachable + the operator has read access",
		})
		finish()
		// We can't persist history without the storage handle;
		// the report itself is the (best-effort) record.
		return report, nil
	}
	defer sp.Close()

	// Persist a slim history entry on the way out unless the
	// operator opted out OR the open-repo phase failed before
	// we got a storage handle.  We do this in a deferred closure
	// so a panic between here and finish() doesn't drop the
	// history record on the floor (the supervisor catches the
	// panic; the deferred persist runs first).
	if !opts.SkipHistory {
		defer func() {
			// finish() already populated StoppedAt + DurationMS.
			entry := SummariseDrillReport(report, opts.Operator)
			if entry == nil {
				return
			}
			// Best-effort persist; a history-write failure
			// shouldn't fail the drill verdict — a critical
			// drill failure with no history is still a useful
			// report.
			_ = NewHistoryStore(sp).Append(ctx, entry)
		}()
	}

	// Phase 1: pick the manifest.
	pickStart := time.Now().UTC()
	manifest, err := pickDrillManifest(ctx, sp, deployment, opts)
	pickPhase := DrillPhase{
		Name:      "pick",
		StartedAt: pickStart,
		StoppedAt: time.Now().UTC(),
	}
	pickPhase.DurationMS = pickPhase.StoppedAt.Sub(pickPhase.StartedAt).Milliseconds()
	if err != nil {
		pickPhase.OK = false
		pickPhase.Error = err.Error()
		report.Phases = append(report.Phases, pickPhase)
		report.Issues = append(report.Issues, ReadinessIssue{
			Severity:   SeverityCritical,
			Code:       "recovery.drill_pick_failed",
			Message:    err.Error(),
			Suggestion: "ensure the deployment has at least one committed backup; run `pg_hardstorage list " + deployment + "`",
		})
		finish()
		return report, nil
	}
	pickPhase.OK = true
	pickPhase.Note = fmt.Sprintf("picked %s (%s, %d files)",
		manifest.BackupID, manifest.Type, len(manifest.Files))
	report.Phases = append(report.Phases, pickPhase)
	report.BackupID = manifest.BackupID

	// Phase 2: prepare the temp target dir.
	prepStart := time.Now().UTC()
	targetDir, prepErr := prepareDrillDir(opts.TempBaseDir, deployment)
	prepPhase := DrillPhase{
		Name:      "prepare",
		StartedAt: prepStart,
		StoppedAt: time.Now().UTC(),
	}
	prepPhase.DurationMS = prepPhase.StoppedAt.Sub(prepPhase.StartedAt).Milliseconds()
	if prepErr != nil {
		prepPhase.OK = false
		prepPhase.Error = prepErr.Error()
		report.Phases = append(report.Phases, prepPhase)
		report.Issues = append(report.Issues, ReadinessIssue{
			Severity:   SeverityCritical,
			Code:       "recovery.drill_prepare_failed",
			Message:    prepErr.Error(),
			Suggestion: "the operator's TMPDIR may be full or read-only",
		})
		finish()
		return report, nil
	}
	prepPhase.OK = true
	prepPhase.Note = "target dir " + targetDir
	report.Phases = append(report.Phases, prepPhase)
	report.TargetDir = targetDir

	// Always tear down on the way out unless KeepTargetDir.
	if !opts.KeepTargetDir {
		defer func() {
			tdStart := time.Now().UTC()
			rmErr := os.RemoveAll(targetDir)
			tdPhase := DrillPhase{
				Name:      "teardown",
				StartedAt: tdStart,
				StoppedAt: time.Now().UTC(),
			}
			tdPhase.DurationMS = tdPhase.StoppedAt.Sub(tdPhase.StartedAt).Milliseconds()
			if rmErr != nil {
				tdPhase.OK = false
				tdPhase.Error = rmErr.Error()
			} else {
				tdPhase.OK = true
			}
			report.Phases = append(report.Phases, tdPhase)
			// Clear TargetDir from the report — it doesn't exist
			// any more, no point pointing the operator at it.
			report.TargetDir = ""
		}()
	}

	// Phase 3: restore.
	restoreStart := time.Now().UTC()
	restoreFn := opts.restoreFn
	if restoreFn == nil {
		restoreFn = restore.Restore
	}
	res, restoreErr := restoreFn(ctx, restore.Options{
		RepoURL:    repoURL,
		Deployment: deployment,
		BackupID:   manifest.BackupID,
		TargetDir:  targetDir,
		Verifier:   opts.Verifier,
		KEKForRef:  opts.KEKResolver,
		UnwrapDEK:  opts.DEKUnwrapper,
		OnEvent:    opts.OnEvent,
		Actor:      "recovery-drill",
	})
	restorePhase := DrillPhase{
		Name:      "restore",
		StartedAt: restoreStart,
		StoppedAt: time.Now().UTC(),
	}
	restorePhase.DurationMS = restorePhase.StoppedAt.Sub(restorePhase.StartedAt).Milliseconds()
	if restoreErr != nil {
		restorePhase.OK = false
		restorePhase.Error = restoreErr.Error()
		report.Phases = append(report.Phases, restorePhase)
		report.Issues = append(report.Issues, ReadinessIssue{
			Severity:   SeverityCritical,
			Code:       "recovery.drill_restore_failed",
			Message:    restoreErr.Error(),
			Suggestion: "investigate via `pg_hardstorage verify " + deployment + " " + manifest.BackupID + "`; possible chunk-store corruption or KEK resolution failure",
		})
		finish()
		return report, nil
	}
	restorePhase.OK = true
	restorePhase.Note = fmt.Sprintf("restored %d files (%d bytes, %d chunks)",
		res.FileCount, res.BytesWritten, res.ChunksFetched)
	report.Phases = append(report.Phases, restorePhase)
	report.Restore = res
	report.RTOActualSeconds = int64(time.Since(started).Seconds())

	// Phase 4: verify (sandbox).
	if opts.SkipVerifyEntirely {
		report.Verdict = DrillVerdictPartial
		report.Issues = append(report.Issues, ReadinessIssue{
			Severity:   SeverityNotice,
			Code:       "recovery.drill_verify_skipped_explicitly",
			Message:    "verify phase skipped (--skip-verify)",
			Suggestion: "the restore succeeded structurally but pg_verifybackup wasn't run; treat the verdict as advisory",
		})
		finish()
		return report, nil
	}
	verifyStart := time.Now().UTC()
	verifyFn := opts.verifyFn
	if verifyFn == nil {
		verifyFn = sandbox.Verify
	}
	pgMajor := opts.PGMajor
	if pgMajor == "" {
		pgMajor = fmt.Sprintf("%d", manifest.PGVersion)
	}
	verifyRes, verifyErr := verifyFn(ctx, sandbox.Options{
		DataDir: targetDir,
		PGMajor: pgMajor,
		Image:   opts.SandboxImage,
	})
	verifyPhase := DrillPhase{
		Name:      "verify",
		StartedAt: verifyStart,
		StoppedAt: time.Now().UTC(),
	}
	verifyPhase.DurationMS = verifyPhase.StoppedAt.Sub(verifyPhase.StartedAt).Milliseconds()
	if verifyErr != nil {
		verifyPhase.OK = false
		verifyPhase.Error = verifyErr.Error()
		report.Phases = append(report.Phases, verifyPhase)
		report.Verdict = DrillVerdictFail
		report.Issues = append(report.Issues, ReadinessIssue{
			Severity:   SeverityCritical,
			Code:       "recovery.drill_verify_errored",
			Message:    verifyErr.Error(),
			Suggestion: "Docker may be unavailable on the operator host; rerun with --skip-verify or --allow-skip-verify when the sandbox is unavailable",
		})
		finish()
		return report, nil
	}
	report.Verify = verifyRes
	switch {
	case verifyRes.Skipped:
		// pg_verifybackup couldn't run (e.g. backup_manifest
		// missing).  Convert to Partial when allowed; Fail
		// otherwise.
		verifyPhase.OK = opts.AllowSkipVerify
		verifyPhase.Note = "skipped: " + verifyRes.SkipReason
		report.Phases = append(report.Phases, verifyPhase)
		if opts.AllowSkipVerify {
			report.Verdict = DrillVerdictPartial
			report.Issues = append(report.Issues, ReadinessIssue{
				Severity:   SeverityWarning,
				Code:       "recovery.drill_verify_skipped",
				Message:    "pg_verifybackup skipped: " + verifyRes.SkipReason,
				Suggestion: "the source manifest may pre-date the+ backup_manifest field; take a fresh backup to anchor a clean drill window",
			})
		} else {
			report.Verdict = DrillVerdictFail
			report.Issues = append(report.Issues, ReadinessIssue{
				Severity:   SeverityCritical,
				Code:       "recovery.drill_verify_skipped",
				Message:    "pg_verifybackup skipped: " + verifyRes.SkipReason,
				Suggestion: "rerun with --allow-skip-verify if the source manifest's missing backup_manifest is acceptable; otherwise re-anchor with a fresh full backup",
			})
		}
	case !verifyRes.Passed:
		verifyPhase.OK = false
		verifyPhase.Note = "pg_verifybackup reported a failure"
		report.Phases = append(report.Phases, verifyPhase)
		report.Verdict = DrillVerdictFail
		report.Issues = append(report.Issues, ReadinessIssue{
			Severity:   SeverityCritical,
			Code:       "recovery.drill_verify_failed",
			Message:    "pg_verifybackup detected a problem in the restored data dir",
			Suggestion: "review the verify stdout/stderr in the report; consider running `pg_hardstorage verify " + deployment + " " + manifest.BackupID + " --full` for the full diagnostic",
		})
	default:
		verifyPhase.OK = true
		verifyPhase.Note = fmt.Sprintf("pg_verifybackup passed (image %s)", verifyRes.Image)
		report.Phases = append(report.Phases, verifyPhase)
		report.Verdict = DrillVerdictPass
	}

	finish()
	return report, nil
}

// pickDrillManifest resolves the BackupID to a full Manifest.
// "latest" / empty picks the freshest StoppedAt.
func pickDrillManifest(ctx context.Context, sp storage.StoragePlugin, deployment string, opts DrillOptions) (*backup.Manifest, error) {
	store := backup.NewManifestStore(sp)
	target := opts.BackupID
	if target == "" || target == "latest" {
		var newest *backup.Manifest
		for m, lerr := range store.List(ctx, deployment, opts.Verifier) {
			if lerr != nil {
				continue
			}
			if newest == nil || m.StoppedAt.After(newest.StoppedAt) {
				newest = m
			}
		}
		if newest == nil {
			return nil, fmt.Errorf("recovery: drill: no usable backups for deployment %q", deployment)
		}
		return newest, nil
	}
	m, err := store.Read(ctx, deployment, target, opts.Verifier)
	if err != nil {
		return nil, fmt.Errorf("recovery: drill: read manifest %s/%s: %w",
			deployment, target, err)
	}
	return m, nil
}

// prepareDrillDir creates a fresh temporary directory under base.
// Empty base → os.TempDir().  The directory's name embeds the
// deployment + a timestamp so concurrent drills don't collide.
func prepareDrillDir(base, deployment string) (string, error) {
	if base == "" {
		base = os.TempDir()
	}
	pattern := fmt.Sprintf("pg_hardstorage-drill-%s-*", deployment)
	dir, err := os.MkdirTemp(base, pattern)
	if err != nil {
		return "", fmt.Errorf("mkdir-temp: %w", err)
	}
	// MkdirTemp uses 0700 — exactly what we want for the data
	// directory of a transient PG cluster.
	if err := os.Chmod(dir, 0o700); err != nil {
		// MkdirTemp already created with 0700; this is a
		// belt-and-suspenders for filesystems that override
		// (e.g. tmpfs with default umask).
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("chmod-700: %w", err)
	}
	return dir, nil
}

// PhaseByName returns the named phase from the report, or an
// empty zero-value if absent.  Helper for renderers that want to
// reference a specific phase without iterating.
func (r *DrillReport) PhaseByName(name string) DrillPhase {
	for _, p := range r.Phases {
		if p.Name == name {
			return p
		}
	}
	return DrillPhase{}
}

// AnyPhaseFailed reports whether any non-teardown phase reports
// OK=false.  Teardown failures are operationally noisy but don't
// invalidate the drill verdict.
func (r *DrillReport) AnyPhaseFailed() bool {
	for _, p := range r.Phases {
		if p.Name == "teardown" {
			continue
		}
		if !p.OK {
			return true
		}
	}
	return false
}

// drillTargetCleanup is the sandbox-tear-down helper kept here so
// renderers + tests don't reach into os.RemoveAll directly.  Used
// only when KeepTargetDir is false; the deferred path inside
// Drill calls os.RemoveAll directly because the report's
// Phases slice grows there in-place.  Exposed for tests that
// want to assert a directory is gone after a kept-keep + manual
// cleanup.
func drillTargetCleanup(dir string) error {
	if dir == "" {
		return nil
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("refusing to clean up non-absolute drill dir %q", dir)
	}
	return os.RemoveAll(dir)
}
