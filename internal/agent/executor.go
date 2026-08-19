// executor.go — BackupExecutor: JobBackup runner that wraps deployment config + keystore.
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// BackupExecutor implements JobExecutor for JobBackup. It wraps the
// agent's local config (which knows each deployment's pg_connection,
// tenant, etc.) plus the loaded keystore (signer + verifier).
//
// On a JobBackup, the executor:
//  1. Looks up the named deployment in the local config.
//  2. Validates the Job's RepoURL matches (or is consistent with)
//     the local config — refuses otherwise so a control plane that
//     pointed an agent at the wrong repo can't write into it.
//  3. Builds runner.TakeOptions and calls runner.Take.
//  4. Forwards each output.Event the runner emits to the
//     control-plane progress callback.
//
// Restore is handled by a sibling RestoreExecutor; the agent's
// RouterExecutor dispatches by Kind.
type BackupExecutor struct {
	deployments map[string]config.DeploymentConfig
	kms         config.KMSConfig
	signer      *backup.Signer
	verifier    *backup.Verifier
}

// NewBackupExecutor constructs an executor with the supplied config
// + keystore. The maps are not copied — callers retain ownership.
//
// kmsCfg is the top-level `kms:` section: it supplies the provider
// settings for whatever `kek_ref` a deployment declares, so a
// control-plane backup can wrap its DEK in a cloud KMS. Zero value =
// no declared providers, which still works for schemes that carry
// everything in the KEKRef itself (pkcs11) or need no config at all.
func NewBackupExecutor(deps map[string]config.DeploymentConfig, kmsCfg config.KMSConfig, signer *backup.Signer, verifier *backup.Verifier) *BackupExecutor {
	return &BackupExecutor{
		deployments: deps,
		kms:         kmsCfg,
		signer:      signer,
		verifier:    verifier,
	}
}

// Execute implements JobExecutor.
func (b *BackupExecutor) Execute(ctx context.Context, job *ControlPlaneJob, progress func(map[string]any)) (map[string]any, error) {
	if job == nil {
		return nil, errors.New("backup-executor: nil job")
	}
	if job.Kind != "backup" {
		// Refuses everything but backup. The router only routes
		// "backup" here; an unexpected kind reaching this method
		// indicates a wiring bug, so we surface it loudly.
		return nil, fmt.Errorf("backup-executor: refusing kind %q (expects backup)", job.Kind)
	}
	return b.runBackup(ctx, job, progress)
}

func (b *BackupExecutor) runBackup(ctx context.Context, job *ControlPlaneJob, progress func(map[string]any)) (map[string]any, error) {
	// Validation order: identity guards (deployment + repo match)
	// before key guards (signer present). An unknown-deployment or
	// repo-mismatch reveals control-plane misconfiguration; we
	// refuse loudly before we even ask for a signing key.
	dep, ok := b.deployments[job.Deployment]
	if !ok {
		return nil, fmt.Errorf("backup-executor: deployment %q not in local config; agent shouldn't have claimed this job", job.Deployment)
	}
	if dep.PGConnection == "" {
		return nil, fmt.Errorf("backup-executor: deployment %q has no pg_connection in local config", job.Deployment)
	}
	repoURL := job.RepoURL
	if repoURL == "" {
		repoURL = dep.Repo
	}
	if repoURL == "" {
		return nil, fmt.Errorf("backup-executor: deployment %q has no repo configured locally and the job didn't supply one", job.Deployment)
	}
	if dep.Repo != "" && !repoMatches(repoURL, dep.Repo) {
		// Refuse cross-repo writes: the control plane should never
		// dispatch a job whose RepoURL diverges from the agent's
		// declared repo. This is a guardrail against control-plane
		// misconfiguration writing into the wrong bucket.
		return nil, fmt.Errorf("backup-executor: deployment %q job repo (%s) doesn't match agent-local repo (%s); refusing", job.Deployment, repoURL, dep.Repo)
	}
	if b.signer == nil || b.verifier == nil {
		return nil, errors.New("backup-executor: signer/verifier not loaded; agent's keystore is missing")
	}

	// Honour Args.fast / Args.label / Args.inactivity_timeout when
	// the operator's POST /v1/deployments/<n>/backups passed them.
	fast := false
	label := ""
	inactivity := 0 * time.Second
	if v, ok := job.Args["fast"].(bool); ok {
		fast = v
	}
	if v, ok := job.Args["label"].(string); ok {
		label = v
	}
	if v, ok := job.Args["inactivity_timeout"].(string); ok {
		if d, err := time.ParseDuration(v); err == nil {
			inactivity = d
		}
	}

	emit := func(ev *output.Event) {
		// Best-effort: a forward-failure doesn't fail the backup.
		body := map[string]any{
			"severity_name": ev.SeverityName,
			"component":     ev.Component,
			"op":            ev.Op,
		}
		if ev.Body != nil {
			body["body"] = ev.Body
		}
		if ev.Suggestion != nil {
			body["suggestion"] = ev.Suggestion
		}
		progress(body)
	}

	// Resolve encryption through the same resolver the interactive CLI
	// uses: the deployment's kek_ref when it declares one, else a KEK
	// on the keyring ⇒ encrypt. Without this, control-plane backups
	// were silently plaintext even in an --encrypt repo, and
	// plaintext-hash dedup welds those manifests onto encrypted chunks.
	var enc *runner.EncryptionConfig
	if p, perr := paths.Resolve(paths.DefaultOptions()); perr == nil {
		var eerr error
		enc, eerr = runner.ResolveEncryption(ctx, runner.EncryptionRequest{
			KeyringDir: p.Keyring.Value,
			KEKRef:     dep.KEKRef,
			KMSConfig:  b.kms.ProviderConfig(dep.KEKRef),
		})
		if eerr != nil {
			return nil, fmt.Errorf("backup-executor: %w", eerr)
		}
	}
	// The provider holds SDK connection state; close it once the
	// backup has wrapped its DEK.
	if enc != nil && enc.Provider != nil {
		defer enc.Provider.Close()
	}

	res, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString:      dep.PGConnection,
		RepoURL:           repoURL,
		Deployment:        job.Deployment,
		Tenant:            dep.Tenant,
		Signer:            b.signer,
		Verifier:          b.verifier,
		Label:             label,
		Fast:              fast,
		InactivityTimeout: inactivity,
		OnEvent:           emit,
		Encryption:        enc,
		SkipLease:         dep.AllowUnenforceableLease,
		// Actor in the audit chain: the dispatch path uses the
		// agent-id-on-job, distinguishing scheduler-driven backups
		// from operator-initiated ones.
		Actor: "agent:job:" + job.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("backup-executor: runner.Take: %w", err)
	}
	out := map[string]any{
		"backup_id":          res.BackupID,
		"deployment":         res.Deployment,
		"start_lsn":          res.StartLSN,
		"stop_lsn":           res.StopLSN,
		"timeline":           res.Timeline,
		"started_at":         res.StartedAt.Format(time.RFC3339Nano),
		"stopped_at":         res.StoppedAt.Format(time.RFC3339Nano),
		"duration_ms":        res.Duration.Milliseconds(),
		"file_count":         res.FileCount,
		"unique_chunk_count": res.UniqueChunkCount,
		"logical_bytes":      res.LogicalBytes,
	}
	return out, nil
}

// repoMatches reports whether two repo URLs reference the same
// repository. Currently a strict string match — same-target with
// different scheme/host expressions is a enhancement (we'd need
// to canonicalise URLs first).
//
// Kept as a separate helper so the policy is in one place when we
// decide to relax it.
func repoMatches(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}
