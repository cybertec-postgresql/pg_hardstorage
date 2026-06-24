// verify_full.go — CLI surface for end-to-end backup verification (restore + PG sanity in a sandbox).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/verify/sandbox"
)

// runVerifyFull is the `verify --full` path. The fast verify path
// (manifest signature + chunk SHA-256 round-trip) tells you the bytes
// are intact; --full goes one step further and asserts the bytes
// reassemble into a PG-recognizable cluster by running the official
// pg_verifybackup tool against a freshly-restored copy in a Docker
// sandbox.
//
// Flow:
//
//  1. Resolve the backup ID (latest → walk).
//  2. Restore into a temp dir on the host (no WAL replay).
//  3. Spin up a postgres:<major> container with the temp dir
//     bind-mounted at /var/lib/postgresql/data:ro.
//  4. Exec pg_verifybackup inside the container.
//  5. Tear down container; remove temp dir; emit Result.
//
// Constraints:
//
//   - Requires a Docker daemon reachable via the standard testcontainers
//     discovery (DOCKER_HOST or /var/run/docker.sock).
//   - The temp dir lives under TMPDIR; for a 100 GB backup that means
//     100 GB of free space. Rejection at the OS level (ENOSPC) bubbles
//     up; we don't pre-check.
//   - PG major version comes from the manifest's pg_version (e.g.
//     170000 → "17"); --pg-major overrides for sandbox images that
//     differ from the source PG.
func runVerifyFull(cmd *cobra.Command, deployment, backupID, repoURL, pgMajorOverride string) error {
	d := DispatcherFrom(cmd)
	// Resolve --repo from the named deployment in config when omitted (#12).
	_, repoURL = deploymentDefaults(deployment, "", repoURL)
	if repoURL == "" {
		return missingFlagErr(cmd, "--repo")
	}

	verifier, err := loadVerifier()
	if err != nil {
		return err
	}
	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	store := backup.NewManifestStore(sp)
	if backupID == "latest" {
		latest, err := pickLatestBackup(cmd.Context(), store, deployment, verifier)
		if err != nil {
			return err
		}
		backupID = latest
	}
	m, err := store.Read(cmd.Context(), deployment, backupID, verifier)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return output.NewError("notfound.backup",
				fmt.Sprintf("verify --full: backup %q for deployment %q not found",
					backupID, deployment)).Wrap(err)
		}
		return output.NewError("verify.read_manifest_failed",
			fmt.Sprintf("verify --full: %v", err)).Wrap(err)
	}

	major := pgMajorOverride
	if major == "" {
		major = pgMajorFromManifestVersion(m.PGVersion)
	}

	// 1. Restore into a temp dir.
	tmp, err := os.MkdirTemp("", "pg_hardstorage-verify-")
	if err != nil {
		return output.NewError("verify.tempdir_failed",
			fmt.Sprintf("verify --full: mkdir tempdir: %v", err)).Wrap(err)
	}
	defer os.RemoveAll(tmp)

	// Resolve the same keystore the rest of the CLI uses.  For
	// unencrypted backups the resolver is consulted only when
	// manifest.Encryption is non-nil, so this is a no-op for
	// plaintext backups and the right resolver for encrypted
	// ones.  Pre-fix, verify --full hard-coded an "unset KEK"
	// stub that refused every encrypted backup before ever
	// reading the manifest's encryption block — even for
	// backups the operator's keystore could trivially unwrap.
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return output.NewError("verify.path_resolve",
			fmt.Sprintf("verify --full: %v", err)).Wrap(err)
	}
	kekResolver := keystore.KEKResolver(p.Keyring.Value)
	kmsCfg, _ := cmd.Flags().GetStringToString("kms-config")

	if _, err := restore.Restore(cmd.Context(), restore.Options{
		RepoURL:    repoURL,
		Deployment: deployment,
		BackupID:   backupID,
		TargetDir:  tmp,
		Verifier:   verifier,
		KEKForRef:  kekResolver,
		UnwrapDEK:  keystore.DEKResolver(p.Keyring.Value, stringMapToAny(kmsCfg)),
	}); err != nil {
		// Distinguish a malformed-manifest failure from a generic
		// restore failure (issue #91).  A manifest.invalid error
		// from a pre-#91-fix build means the manifest committed
		// without passing its own invariant check — the operator
		// needs to retake the backup, not retry the restore.
		ce := output.NewError("verify.restore_failed",
			fmt.Sprintf("verify --full: restore into sandbox: %v", err)).Wrap(err)
		if strings.Contains(err.Error(), "manifest.invalid") ||
			strings.Contains(err.Error(), "backup_label is empty") {
			ce = ce.WithSuggestion(&output.Suggestion{
				Human: "this backup's manifest fails its own invariant check — it was committed " +
					"before the manifest-validation gate landed (issue #91).  basic `verify` only " +
					"re-hashes chunks; it does not catch this.  Take a fresh backup with " +
					"`pg_hardstorage backup " + deployment + "` (the runner now refuses to commit " +
					"a malformed manifest) and retry `verify --full` against the new backup.",
				Command: "pg_hardstorage backup " + deployment,
			})
		}
		return ce
	}

	// 2. Sandbox verify.
	res, err := sandbox.Verify(cmd.Context(), sandbox.Options{
		DataDir: tmp,
		PGMajor: major,
	})
	if err != nil {
		return output.NewError("verify.sandbox_failed",
			fmt.Sprintf("verify --full: sandbox: %v", err)).
			WithSuggestion(&output.Suggestion{
				Human: "ensure Docker is reachable (testcontainers uses DOCKER_HOST or /var/run/docker.sock)",
			}).Wrap(err)
	}

	body := verifyFullBody{
		Deployment: deployment,
		BackupID:   backupID,
		PGMajor:    res.PGMajor,
		Image:      res.Image,
		Tool:       res.Tool,
		Passed:     res.Passed,
		Skipped:    res.Skipped,
		SkipReason: res.SkipReason,
		DurationMS: res.Duration.Milliseconds(),
		ToolStdout: res.Stdout,
	}
	// Record the full verification run in the audit chain as `verify.run`
	// (mode=full) so the compliance verification section rolls it up.
	// Best-effort — a chain-write failure must not fail the verify.
	fullOutcome := "ok"
	if res.Skipped {
		fullOutcome = "skipped"
	} else if !res.Passed {
		fullOutcome = "failed"
	}
	audit.NewStoreWithRetention(sp, repoMeta.WORM).AppendOrLog(cmd.Context(), &audit.Event{
		Action:  "verify.run",
		Subject: audit.Subject{Deployment: deployment, BackupID: backupID, Repo: repoURL},
		Body:    map[string]any{"outcome": fullOutcome, "mode": "full"},
	})

	if !res.Passed && !res.Skipped {
		// Treat as exit code 9 (verify failure).
		return output.NewError("verify.failed",
			fmt.Sprintf("verify --full: pg_verifybackup reported failure (see tool_stdout in body)")).
			WithSuggestion(&output.Suggestion{
				Human: "the backup's bytes are intact (fast verify passed) but pg_verifybackup found a discrepancy. Re-run with --output json to capture the full tool output for triage.",
			})
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// resolveKEKForVerify is the package-shared default KEK resolver
// used by verify --full, standby create, and timetravel restore.
// It wraps the operator's keystore (resolved via paths.Resolve),
// so an encrypted backup decrypts under the same KEK the agent
// would use.
//
// Before this was wired (issue #98-class drift), this function
// returned an unset-KEK sentinel that refused every encrypted
// backup — `verify --full <encrypted-backup>` failed before
// reading the manifest's encryption block.
func resolveKEKForVerify(ref string) ([encryption.KeyLen]byte, error) {
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		var zero [encryption.KeyLen]byte
		return zero, fmt.Errorf("verify: resolve keystore paths: %w", err)
	}
	return keystore.KEKResolver(p.Keyring.Value)(ref)
}

// resolveDEKForVerify is the package-shared cloud-capable DEK resolver used
// by verify --full, standby create, and timetravel restore for
// restore.Options.UnwrapDEK (issue #102). The restore engine calls it only
// for cloud KEKRefs (the KEK never leaves the HSM), so a failure to resolve
// the local keyring path is benign here — cloud unwrap doesn't use it.
func resolveDEKForVerify(ctx context.Context, kekRef string, wrapped []byte) ([]byte, error) {
	keyringDir := ""
	if p, err := paths.Resolve(paths.DefaultOptions()); err == nil {
		keyringDir = p.Keyring.Value
	}
	return keystore.UnwrapDEK(ctx, kekRef, wrapped, keystore.UnwrapOpts{KeyringDir: keyringDir})
}

// pgMajorFromManifestVersion extracts the major from PG's
// numeric_version: PG_VERSION_NUM convention is `MMmmpp` where MM is
// the major (e.g. 17), mm the minor, pp the patch. PG 10+ collapsed
// the second digit.
//
// Falls back to pg.DefaultSandboxMajor (the current upstream-stable
// major) when v is zero or otherwise unparseable. Single source of
// truth: bumping the default in the pg package propagates here
// without ad-hoc hardcodes.
func pgMajorFromManifestVersion(v int) string {
	fallback := strconv.Itoa(pg.DefaultSandboxMajor)
	if v <= 0 {
		return fallback
	}
	major := v / 10000
	if major <= 0 {
		return fallback
	}
	return strconv.Itoa(major)
}

// verifyFullBody is the JSON body for `verify --full` success.
type verifyFullBody struct {
	Deployment string `json:"deployment"`
	BackupID   string `json:"backup_id"`
	PGMajor    string `json:"pg_major"`
	Image      string `json:"image"`
	Tool       string `json:"tool"`
	Passed     bool   `json:"passed"`
	Skipped    bool   `json:"skipped,omitempty"`
	SkipReason string `json:"skip_reason,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	ToolStdout string `json:"tool_stdout,omitempty"`
}

// WriteText renders the verify --full outcome — passed/skipped/failed plus
// tool output — as human-readable text to w.
func (b verifyFullBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if b.Skipped {
		fmt.Fprintf(bw, "○ verify --full skipped\n  Reason: %s\n", b.SkipReason)
	} else if b.Passed {
		fmt.Fprintf(bw, "✓ verify --full passed (%s on %s)\n", b.Tool, b.Image)
	} else {
		fmt.Fprintf(bw, "✗ verify --full FAILED (%s on %s)\n", b.Tool, b.Image)
	}
	fmt.Fprintf(bw, "  Deployment:  %s\n", b.Deployment)
	fmt.Fprintf(bw, "  Backup:      %s\n", b.BackupID)
	fmt.Fprintf(bw, "  Duration:    %d ms\n", b.DurationMS)
	if b.ToolStdout != "" {
		fmt.Fprintf(bw, "  Tool output:\n    %s",
			strings.ReplaceAll(strings.TrimRight(b.ToolStdout, "\n"), "\n", "\n    "))
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
