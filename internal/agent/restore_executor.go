// restore_executor.go — RestoreExecutor: JobRestore runner with PITR + repo-match guardrails.
package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/naturaltime"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/walfetchcmd"
)

// RestoreExecutor implements JobExecutor for JobRestore. It mirrors
// BackupExecutor in shape: deployment lookup → repo-match guardrail →
// keystore resolution → restore.Restore call. Progress events from
// the restore orchestrator forward through the control-plane progress
// callback the same way backup events do.
//
// Body shape (set by handleEnqueueRestore on the server side):
//
//	{
//	  "backup_id":  "db1.full.20260427T0900Z" | "latest",
//	  "target_dir": "/var/lib/postgresql/restored",
//	  "allow_overwrite": false,
//	  "to":         "5 minutes ago",          // optional PITR
//	  "to_lsn":     "0/3000028",              // optional PITR
//	  "to_name":    "before_drop",            // optional PITR
//	  "to_action":  "pause"|"promote"|"shutdown",
//	  "to_timeline": "latest"|"<n>",
//	  "to_inclusive": true,                   // PG default: true
//	  "verify_after": true                    // pg_verifybackup gate
//	}
//
// At most one of to / to_lsn / to_name may be set; the executor
// surfaces a structured error otherwise so the operator gets a
// parseable code rather than a half-finished restore.
type RestoreExecutor struct {
	deployments map[string]config.DeploymentConfig
	verifier    *backup.Verifier
	keyringDir  string
}

// NewRestoreExecutor constructs an executor with the supplied config
// + verifier + keyring path. The keyringDir flows into a KEKResolver
// that the restore orchestrator calls when the manifest is encrypted;
// for unencrypted manifests it's never consulted, so an empty
// keyringDir doesn't fail unencrypted restores.
func NewRestoreExecutor(deps map[string]config.DeploymentConfig, verifier *backup.Verifier, keyringDir string) *RestoreExecutor {
	return &RestoreExecutor{
		deployments: deps,
		verifier:    verifier,
		keyringDir:  keyringDir,
	}
}

// Execute implements JobExecutor.
func (e *RestoreExecutor) Execute(ctx context.Context, job *ControlPlaneJob, progress func(map[string]any)) (map[string]any, error) {
	if job == nil {
		return nil, errors.New("restore-executor: nil job")
	}
	if job.Kind != "restore" {
		return nil, fmt.Errorf("restore-executor: refusing kind %q (expects restore)", job.Kind)
	}

	dep, ok := e.deployments[job.Deployment]
	if !ok {
		return nil, fmt.Errorf("restore-executor: deployment %q not in local config; agent shouldn't have claimed this job", job.Deployment)
	}
	repoURL := job.RepoURL
	if repoURL == "" {
		repoURL = dep.Repo
	}
	if repoURL == "" {
		return nil, fmt.Errorf("restore-executor: deployment %q has no repo configured locally and the job didn't supply one", job.Deployment)
	}
	// Cross-repo refusal — same posture as BackupExecutor. We will
	// not read from a repo other than the agent's declared one, so
	// a misconfigured control plane can't redirect a restore to an
	// untrusted source.
	if dep.Repo != "" && !repoMatches(repoURL, dep.Repo) {
		return nil, fmt.Errorf("restore-executor: deployment %q job repo (%s) doesn't match agent-local repo (%s); refusing", job.Deployment, repoURL, dep.Repo)
	}
	if e.verifier == nil {
		return nil, errors.New("restore-executor: verifier not loaded; agent's keystore is missing")
	}

	backupID, _ := job.Args["backup_id"].(string)
	if backupID == "" {
		return nil, errors.New("restore-executor: backup_id is required")
	}
	targetDir, _ := job.Args["target_dir"].(string)
	if targetDir == "" {
		return nil, errors.New("restore-executor: target_dir is required")
	}

	// Tablespace remapping. The control-plane body carries
	// tablespace_mapping as a JSON array of "OLD=NEW" strings (the same
	// shape the CLI's --tablespace-mapping produces). Parse + validate
	// it here, early, so a malformed entry fails fast before the repo
	// round-trip — and, critically, so it actually reaches
	// restore.Restore. Omitting this silently dropped the operator's
	// remap on every control-plane restore, materialising the cluster
	// at the manifest's original tablespace paths (which is exactly the
	// situation --tablespace-mapping exists to avoid).
	tsRemap, err := parseTablespaceMappingArg(job.Args["tablespace_mapping"])
	if err != nil {
		return nil, err
	}

	// Parse the `to` time target ONCE, here, and reuse it for both seed
	// resolution and the armed recovery_target_time (buildRecoveryFromArgs).
	// Parsing it twice with separate references risks the bare-clock
	// "today/yesterday HH:MM" zone-drift the CLI hit; one parse, one
	// instant, no drift. Zero Time when `to` is absent.
	var targetTime time.Time
	if s, _ := job.Args["to"].(string); s != "" {
		t, perr := naturaltime.Parse(s, time.Now())
		if perr != nil {
			return nil, fmt.Errorf("restore-executor: parse `to`: %w", perr)
		}
		targetTime = t
	}

	// Resolve "latest" by reaching into the repo. The CLI does this
	// at parse-time; we replicate it here so the operator can POST
	// {"backup_id": "latest"} and let the agent figure it out at
	// claim-time (the latest may have changed between enqueue and
	// claim, especially under heavy backup cadence).
	//
	// With a `to` time target set, "latest" must resolve to the most
	// recent backup whose stop_time ≤ target — NOT the unconstrained
	// latest. PG replays WAL strictly forward from the seed's
	// checkpoint, so a seed taken AFTER the target can never reach it
	// (recovery would have to run backwards) and PG aborts startup.
	// This mirrors the CLI's time-aware auto-resolution; without it a
	// control-plane `latest` + `--to <past>` picked a too-new seed and
	// the restored cluster failed to start.
	if backupID == "latest" {
		_, sp, err := repo.Open(ctx, repoURL)
		if err != nil {
			return nil, fmt.Errorf("restore-executor: open repo: %w", err)
		}
		var id string
		if !targetTime.IsZero() {
			id, err = restore.ResolveBackupForTime(ctx, sp, job.Deployment, targetTime, e.verifier)
		} else {
			id, err = restore.ResolveLatest(ctx, sp, job.Deployment, e.verifier)
		}
		sp.Close()
		if err != nil {
			return nil, fmt.Errorf("restore-executor: resolve latest: %w", err)
		}
		backupID = id
	}

	allowOverwrite, _ := job.Args["allow_overwrite"].(bool)

	rec, err := buildRecoveryFromArgs(job.Args, targetTime)
	if err != nil {
		return nil, err
	}
	// Wire the agent's own wal-fetch shim as restore_command. A PITR
	// Recovery (Enable=true) MUST carry a non-empty RestoreCommand:
	// restore.WriteRecoveryFiles → validateRecovery rejects an empty
	// one ("RestoreCommand is required when Enable=true"), so without
	// this every control-plane PITR restore failed with
	// restore.recovery_write. The agent knows its own binary, the
	// deployment, and the resolved repo URL — the same three inputs the
	// CLI uses in buildRestoreCommandString and the non-PITR
	// WriteAutoRecovery path use. PG will invoke this binary as
	// `<agent> wal fetch <deployment> %f %p --repo <url>` for each WAL
	// segment during recovery.
	if rec != nil && rec.Enable {
		bin, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("restore-executor: locate own binary for restore_command: %w", err)
		}
		rec.RestoreCommand = walfetchcmd.Build(bin, job.Deployment, repoURL)
	}

	// KEK resolver. Built every time so a keyring rotation lands
	// without restarting the agent. Empty keyringDir is a valid
	// configuration when the agent only handles unencrypted backups
	// — the resolver will be called only when the manifest's
	// EncryptionInfo is non-nil.
	var kekFor func(ref string) ([encryption.KeyLen]byte, error)
	if e.keyringDir != "" {
		kekFor = keystore.KEKResolver(e.keyringDir)
	}

	emit := func(ev *output.Event) {
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

	res, err := restore.Restore(ctx, restore.Options{
		RepoURL:         repoURL,
		Deployment:      job.Deployment,
		BackupID:        backupID,
		TargetDir:       targetDir,
		Verifier:        e.verifier,
		AllowOverwrite:  allowOverwrite,
		Recovery:        rec,
		TablespaceRemap: tsRemap,
		KEKForRef:       kekFor,
		// Cloud-KMS-encrypted backups unwrap the DEK server-side; the agent
		// relies on the host's ambient cloud credentials (issue #102).
		UnwrapDEK: keystore.DEKResolver(e.keyringDir, nil),
		OnEvent:   emit,
		// Actor in the audit chain: dispatched via control-plane,
		// tagged with the job ID so a forensic walk of the chain
		// can reconstruct who initiated the restore.
		Actor: "agent:job:" + job.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("restore-executor: restore.Restore: %w", err)
	}

	out := map[string]any{
		"backup_id":           res.BackupID,
		"deployment":          res.Deployment,
		"target_dir":          res.TargetDir,
		"file_count":          res.FileCount,
		"bytes_written":       res.BytesWritten,
		"chunks_fetched":      res.ChunksFetched,
		"backup_label_size":   res.BackupLabelSize,
		"tablespace_map_size": res.TablespaceMapSize,
		"started_at":          res.StartedAt.Format(time.RFC3339Nano),
		"stopped_at":          res.StoppedAt.Format(time.RFC3339Nano),
		"duration_ms":         res.Duration.Milliseconds(),
	}
	if rec != nil && rec.Enable {
		out["recovery_configured"] = true
		if rec.TargetLSN != "" {
			out["recovery_target_lsn"] = rec.TargetLSN
		}
		if !rec.TargetTime.IsZero() {
			out["recovery_target_time"] = rec.TargetTime.Format(time.RFC3339)
		}
		if rec.TargetName != "" {
			out["recovery_target_name"] = rec.TargetName
		}
	}
	return out, nil
}

// buildRecoveryFromArgs translates the JSON args into a *Recovery.
// Returns nil when no PITR target was set — callers must treat that
// as "full restore, no recovery configuration." Same one-target rule
// the CLI enforces; we re-validate here so a malformed control-plane
// payload doesn't half-configure recovery.
// targetTime is the pre-parsed `to` instant (parsed once by Execute so
// the seed backup and the armed recovery_target_time share one
// instant). Zero Time when `to` was not set.
func buildRecoveryFromArgs(args map[string]any, targetTime time.Time) (*restore.Recovery, error) {
	toLSN, _ := args["to_lsn"].(string)
	toTime, _ := args["to"].(string)
	toName, _ := args["to_name"].(string)
	hasLSN := toLSN != ""
	hasTime := toTime != ""
	hasName := toName != ""
	if !hasLSN && !hasTime && !hasName {
		// No PITR target → caller wants a plain full restore.
		// Skip recovery configuration entirely; PG will start as a
		// fresh primary.
		return nil, nil
	}
	count := 0
	if hasLSN {
		count++
	}
	if hasTime {
		count++
	}
	if hasName {
		count++
	}
	if count > 1 {
		return nil, errors.New("restore-executor: at most one of to, to_lsn, to_name may be set")
	}
	r := &restore.Recovery{
		Enable: true,
	}
	// Inclusive defaults to true (matches PG's own default + CLI's
	// rendering). Operator opts out via "to_inclusive": false.
	if v, ok := args["to_inclusive"].(bool); ok {
		r.Inclusive = v
	} else {
		r.Inclusive = true
	}
	if v, ok := args["to_action"].(string); ok {
		switch v {
		case "", "pause", "promote", "shutdown":
			r.Action = v
		default:
			return nil, fmt.Errorf("restore-executor: to_action %q must be one of pause|promote|shutdown", v)
		}
	}
	if v, ok := args["to_timeline"].(string); ok {
		if v != "" && v != "latest" {
			if _, err := parsePositiveUint32(v); err != nil {
				return nil, fmt.Errorf("restore-executor: to_timeline %q: must be \"latest\" or a positive integer", v)
			}
		}
		r.Timeline = v
	}
	switch {
	case hasLSN:
		if !restore.LooksLikeLSN(toLSN) {
			return nil, fmt.Errorf("restore-executor: to_lsn %q: expected PG LSN hex form like 0/3000028", toLSN)
		}
		r.TargetLSN = toLSN
	case hasTime:
		// Already parsed by Execute (shared with seed resolution).
		r.TargetTime = targetTime
	case hasName:
		r.TargetName = toName
	}
	// RestoreCommand is left empty HERE and populated by the caller
	// (RestoreExecutor.Execute) once the repo URL is resolved — it
	// builds `<agent> wal fetch <deployment> %f %p --repo <url>` from
	// os.Executable(). It must be non-empty for a PITR Recovery:
	// restore.WriteRecoveryFiles → validateRecovery rejects an empty
	// RestoreCommand when Enable=true. Keeping the wiring in Execute
	// avoids threading repoURL/binary through this pure args translator.
	return r, nil
}

func parsePositiveUint32(s string) (uint32, error) {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, errors.New("must be > 0")
	}
	return uint32(n), nil
}

// parseTablespaceMappingArg converts the control-plane
// tablespace_mapping arg into a validated TablespaceRemap. The JSON
// body carries it as an array of "OLD=NEW" strings, which decodes to
// []any (each a string); we also accept a native []string for
// in-process callers. Returns nil for an absent/empty value. Validation
// (absolute paths, no duplicates, no control characters) runs through
// restore.ParseTablespaceRemap — the same gate the CLI uses.
func parseTablespaceMappingArg(raw any) (restore.TablespaceRemap, error) {
	if raw == nil {
		return nil, nil
	}
	var entries []string
	switch v := raw.(type) {
	case []string:
		entries = v
	case []any:
		for i, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("restore-executor: tablespace_mapping[%d] is not a string (%T)", i, e)
			}
			entries = append(entries, s)
		}
	default:
		return nil, fmt.Errorf("restore-executor: tablespace_mapping must be an array of \"OLD=NEW\" strings, got %T", raw)
	}
	rm, err := restore.ParseTablespaceRemap(entries)
	if err != nil {
		return nil, fmt.Errorf("restore-executor: %w", err)
	}
	return rm, nil
}
