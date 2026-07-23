// verify_executor.go — VerifyExecutor: JobVerify runner that restores into a Docker sandbox and runs pg_verifybackup.
package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/obs/metrics"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/verify/sandbox"
)

// VerifyExecutor implements JobExecutor for JobVerify. The flow
// mirrors the local `verify --full` CLI path: restore the named
// backup into a temp dir, run the official pg_verifybackup tool
// inside a Docker sandbox, return Pass/Fail with the tool's stdout
// for triage, then tear everything down.
//
// Body shape:
//
//	{
//	  "backup_id": "db1.full.…|latest",
//	  "pg_major":  "17",          // optional override; otherwise inferred from manifest
//	  "tempdir":   "/var/lib/pg_hardstorage/verify-tmp"   // optional; defaults to os.TempDir
//	}
//
// Constraints:
//   - The agent host must have Docker reachable (testcontainers picks
//     up DOCKER_HOST or /var/run/docker.sock). Surface a clear
//     suggestion when it's not, so the operator's CLI sees an
//     actionable error rather than a generic test failure.
//   - The temp dir holds a full restore of the backup; large backups
//     need correspondingly large free space. The executor refuses to
//     start when the configured tempdir is on a tmpfs smaller than
//     the manifest's logical size + 10% headroom — a+
//     enhancement; lets the OS surface ENOSPC and the manifest
//     stays uncorrupted because we never write to the source repo.
type VerifyExecutor struct {
	deployments map[string]config.DeploymentConfig
	verifier    *backup.Verifier
	keyringDir  string
}

// NewVerifyExecutor constructs an executor with the supplied config
// + verifier + keyring path. KEK resolution is identical to the
// restore executor's: only consulted for encrypted manifests.
func NewVerifyExecutor(deps map[string]config.DeploymentConfig, verifier *backup.Verifier, keyringDir string) *VerifyExecutor {
	return &VerifyExecutor{
		deployments: deps,
		verifier:    verifier,
		keyringDir:  keyringDir,
	}
}

// Execute implements JobExecutor.
func (e *VerifyExecutor) Execute(ctx context.Context, job *ControlPlaneJob, progress func(map[string]any)) (map[string]any, error) {
	if job == nil {
		return nil, errors.New("verify-executor: nil job")
	}
	if job.Kind != "verify" {
		return nil, fmt.Errorf("verify-executor: refusing kind %q (expects verify)", job.Kind)
	}

	dep, ok := e.deployments[job.Deployment]
	if !ok {
		return nil, fmt.Errorf("verify-executor: deployment %q not in local config; agent shouldn't have claimed this job", job.Deployment)
	}
	repoURL := job.RepoURL
	if repoURL == "" {
		repoURL = dep.Repo
	}
	if repoURL == "" {
		return nil, fmt.Errorf("verify-executor: deployment %q has no repo configured locally and the job didn't supply one", job.Deployment)
	}
	if dep.Repo != "" && !repoMatches(repoURL, dep.Repo) {
		return nil, fmt.Errorf("verify-executor: deployment %q job repo (%s) doesn't match agent-local repo (%s); refusing", job.Deployment, repoURL, dep.Repo)
	}
	if e.verifier == nil {
		return nil, errors.New("verify-executor: verifier not loaded; agent's keystore is missing")
	}

	backupID, _ := job.Args["backup_id"].(string)
	if backupID == "" {
		return nil, errors.New("verify-executor: backup_id is required")
	}

	progress(map[string]any{"op": "verify.opening_repo"})

	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		return nil, fmt.Errorf("verify-executor: open repo: %w", err)
	}
	defer sp.Close()

	// Resolve "latest" at claim time — same posture as restore.
	if backupID == "latest" {
		id, err := restore.ResolveLatest(ctx, sp, job.Deployment, e.verifier)
		if err != nil {
			return nil, fmt.Errorf("verify-executor: resolve latest: %w", err)
		}
		backupID = id
		progress(map[string]any{"op": "verify.latest_resolved", "body": map[string]any{"backup_id": id}})
	}

	// Read the manifest now so we can derive PG major and surface a
	// structured "backup not found" error before doing anything
	// expensive.
	store := backup.NewManifestStore(sp)
	m, err := store.Read(ctx, job.Deployment, backupID, e.verifier)
	if err != nil {
		return nil, fmt.Errorf("verify-executor: read manifest %s/%s: %w", job.Deployment, backupID, err)
	}

	major, _ := job.Args["pg_major"].(string)
	if major == "" {
		major = pgMajorFromManifestVersion(m.PGVersion)
	}
	tempBase, _ := job.Args["tempdir"].(string)

	// Restore into a temp dir. The agent owns the path so the
	// operator doesn't have to think about target_dir for verify —
	// it's an implementation detail of the verify operation.
	tmp, err := os.MkdirTemp(tempBase, "pg_hardstorage-verify-")
	if err != nil {
		return nil, fmt.Errorf("verify-executor: mkdir tempdir: %w", err)
	}
	defer func() {
		// Best-effort cleanup. A leftover temp dir on a verify failure
		// is recoverable (operator can rm -rf); a hung verify is
		// worse, so we don't block on this.
		_ = os.RemoveAll(tmp)
	}()

	progress(map[string]any{
		"op": "verify.restore_started",
		"body": map[string]any{
			"backup_id": backupID,
			"target":    tmp,
			"pg_major":  major,
		},
	})

	var kekFor func(ref string) ([encryption.KeyLen]byte, error)
	if e.keyringDir != "" {
		kekFor = keystore.KEKResolver(e.keyringDir)
	}

	if _, err := restore.Restore(ctx, restore.Options{
		RepoURL:    repoURL,
		Deployment: job.Deployment,
		BackupID:   backupID,
		TargetDir:  tmp,
		Verifier:   e.verifier,
		KEKForRef:  kekFor,
		UnwrapDEK:  keystore.DEKResolver(e.keyringDir, nil),
		// Audit-chain Actor for the inner restore: tags the
		// restore.complete event with "verify:job:<id>" so a forensic
		// walk distinguishes verify-driven sandbox restores from
		// operator-initiated ones (those carry "agent:job:<id>").
		Actor: "verify:job:" + job.ID,
	}); err != nil {
		return nil, fmt.Errorf("verify-executor: restore into sandbox: %w", err)
	}

	progress(map[string]any{
		"op": "verify.sandbox_started",
		"body": map[string]any{
			"image": "postgres:" + major,
		},
	})

	res, err := sandbox.Verify(ctx, sandbox.Options{
		DataDir: tmp,
		PGMajor: major,
	})
	if err != nil {
		// The sandbox itself failed to run (Docker unreachable, image
		// pull error) — the verify never reached a verdict.  Count it
		// as a failed run so a broken verify host is visible on the
		// dashboard, not silently absent.
		metrics.VerifyRun(job.Deployment, "failure")
		return nil, fmt.Errorf("verify-executor: sandbox: %w (ensure Docker is reachable on the agent host)", err)
	}

	// Record the verdict: a skipped verify (no Docker, sampled out) is
	// distinct from a pass or a content failure.
	verifyResult := "success"
	switch {
	case res.Skipped:
		verifyResult = "skipped"
	case !res.Passed:
		verifyResult = "failure"
	}
	metrics.VerifyRun(job.Deployment, verifyResult)

	out := map[string]any{
		"deployment":  job.Deployment,
		"backup_id":   backupID,
		"pg_major":    res.PGMajor,
		"image":       res.Image,
		"tool":        res.Tool,
		"passed":      res.Passed,
		"skipped":     res.Skipped,
		"skip_reason": res.SkipReason,
		"duration_ms": res.Duration.Milliseconds(),
		"started_at":  res.StartedAt.Format(time.RFC3339Nano),
		"stopped_at":  res.StoppedAt.Format(time.RFC3339Nano),
		"tool_stdout": res.Stdout,
		"tool_stderr": res.Stderr,
	}
	if !res.Passed && !res.Skipped {
		// Verify ran successfully but found a problem. We surface
		// this as a job failure with the tool's stdout in the message
		// so the operator's CLI sees a non-zero exit + actionable
		// triage data.
		return out, fmt.Errorf("verify-executor: pg_verifybackup reported failure: %s", trimToTwoLines(res.Stdout))
	}
	return out, nil
}

// pgMajorFromManifestVersion mirrors the helper in
// internal/cli/verify_full.go. Duplicated rather than imported so the
// agent doesn't pull in the cli package. Both fallbacks route
// through pg.DefaultSandboxMajor — single source of truth for the
// "what major do we run sandboxes against by default" answer.
func pgMajorFromManifestVersion(v int) string {
	fallback := fmt.Sprintf("%d", pg.DefaultSandboxMajor)
	if v <= 0 {
		return fallback
	}
	// The runner stores the plain major (pg_version=17); the MMmmpp
	// division applies only to the numeric server_version_num form.
	// Without this, 17/10000=0 routed every scheduled verify into a
	// pg.DefaultSandboxMajor sandbox whose pg_verifybackup rejects
	// older majors' pg_control ("CRC is incorrect").
	if v < 100 {
		return fmt.Sprintf("%d", v)
	}
	major := v / 10000
	if major <= 0 {
		return fallback
	}
	return fmt.Sprintf("%d", major)
}

// trimToTwoLines clips a long pg_verifybackup stdout to the first
// couple of lines. The full tool output rides into the Result body;
// the failure message is just a hint for human eyes.
func trimToTwoLines(s string) string {
	cut := 0
	for i, c := range s {
		if c == '\n' {
			cut++
			if cut == 2 {
				return s[:i]
			}
		}
	}
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
