// restore.go — 'restore' CLI verb: restores a named backup (or 'latest') with PITR options.
package cli

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/attestgate"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/naturaltime"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/walfetchcmd"
)

// LatestKeyword is the literal token a user types to mean "the most
// recent verifiable backup for this deployment".
const LatestKeyword = "latest"

// newRealRestoreCmd is the in-development real restore command.
func newRealRestoreCmd() *cobra.Command {
	var opts restoreOpts
	c := &cobra.Command{
		Use:   "restore <deployment> <backup-id|latest>",
		Short: "Restore a backup to a target directory",
		Long: `Materialise a committed backup at the given target directory,
and optionally arm point-in-time recovery (PITR).

The signing keypair is loaded from the resolved keyring directory
(see ` + "`" + `pg_hardstorage doctor` + "`" + ` for the exact path); the public key
half is what verifies the manifest signature.

Refuses to write into a non-empty target unless --force is passed.
Use --preview to inspect what a real restore would do without
touching disk. Use --verify=auto|skip|require to control the post-
restore pg_verifybackup gate.

PITR (replaying WAL up to a target):
  --to "5 minutes ago"        natural-language relative time
  --to "2026-04-27 09:42 UTC" absolute time, parsed predictably
  --to-lsn 0/3000028          recover up to (and including) this LSN
  --to-name my-restore-point  recover up to a named restore point
  --to-action pause|promote|shutdown   default pause (safest)
  --to-timeline latest|<N>    default 'latest'

When any of --to / --to-lsn / --to-name is set, recovery.signal is
dropped in the target dir and a recovery_target_* block is appended
to postgresql.auto.conf. The restore_command points back at this
binary's wal-fetch shim.`,
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.deployment = args[0]
			opts.backupID = args[1]
			if err := requireBackupIDArg("restore", opts.backupID); err != nil {
				return err
			}
			return runRestore(cmd, opts)
		},
	}
	c.Flags().StringVar(&opts.repoURL, "repo", "",
		"repository URL (file://, s3://, ...) — must already exist (required)")
	c.Flags().StringVar(&opts.targetDir, "target", "",
		"directory where the data dir will be materialised (required)")
	c.Flags().BoolVar(&opts.force, "force", false,
		"allow overwriting a non-empty target directory")
	c.Flags().BoolVar(&opts.forceForeign, "force-foreign", false,
		"with --force, also overwrite a target that is a DIFFERENT cluster (pg_control system identifier mismatch); the default refuses, to guard against a wrong --target path")
	c.Flags().StringVar(&opts.chainStagingRoot, "chain-staging-root", "",
		"directory chain restores use to materialise links (default: derived path under TMPDIR; preserved across retries for resume)")
	c.Flags().StringToStringVar(&opts.kmsConfig, "kms-config", nil,
		"cloud KMS provider config for restoring a cloud-KMS-encrypted backup (e.g. region=eu-central-1,endpoint=...); empty uses ambient credentials")
	c.Flags().BoolVar(&opts.resetChainStaging, "reset-chain-staging", false,
		"wipe the chain-staging directory before starting (force a fresh materialise even if a previous attempt's links are present)")
	c.Flags().BoolVar(&opts.preview, "preview", false,
		"plan the restore but do not write anything")
	c.Flags().StringVar(&opts.verifyMode, "verify", "auto",
		"post-restore pg_verifybackup gate: auto|skip|require")
	c.Flags().StringVar(&opts.verifyRestoreMode, "verify-restore", "auto",
		"post-restore CLUSTER-START smoke test: off|auto|required|dump (catches issue-#7-class "+
			"empty-dirs / permissions / startup failures that pg_verifybackup cannot)")
	c.Flags().StringVar(&opts.toTime, "to", "",
		"recover up to this time (natural language or RFC3339)")
	c.Flags().StringVar(&opts.toLSN, "to-lsn", "",
		"recover up to this LSN (e.g. 0/3000028)")
	c.Flags().StringVar(&opts.toName, "to-name", "",
		"recover up to this PostgreSQL named restore point")
	c.Flags().StringVar(&opts.toAction, "to-action", "pause",
		"action when target reached: pause|promote|shutdown")
	c.Flags().StringVar(&opts.toTimeline, "to-timeline", "latest",
		"target timeline: 'latest' or an explicit TLI number")
	c.Flags().BoolVar(&opts.toExclusive, "to-exclusive", false,
		"stop recovery just BEFORE the target (default: just after)")
	c.Flags().BoolVar(&opts.skipGapCheck, "skip-gap-check", false,
		"bypass the+ WAL-gap pre-flight (operator override; "+
			"the override is audit-logged)")
	c.Flags().StringVar(&opts.requireAttestation, "require-threshold-attestation", "",
		"refuse to restore unless a k-of-n threshold attestation under this roster ID is present "+
			"and pins this manifest's body hash; pairs with `pg_hardstorage threshold attest sign "+
			"backup_manifest <backup-id>`")
	c.Flags().StringArrayVar(&opts.tablespaceMapping, "tablespace-mapping", nil,
		"redirect a tablespace from OLDDIR to NEWDIR (repeatable; both paths must be absolute). "+
			"Plain restores rewrite the manifest's tablespace_map; chain restores pass through to "+
			"pg_combinebackup. Example: --tablespace-mapping=/mnt/ssd/ts_fast=/var/lib/pg/ts_fast")
	registerDispatchFlags(c, &opts.dispatch)
	return c
}

type restoreOpts struct {
	deployment        string
	backupID          string
	repoURL           string
	targetDir         string
	force             bool
	forceForeign      bool
	preview           bool
	verifyMode        string
	verifyRestoreMode string

	// PITR-related flags. At most one of toTime / toLSN / toName may
	// be set; runRestore enforces this.
	toTime      string
	toLSN       string
	toName      string
	toAction    string
	toTimeline  string
	toExclusive bool

	// tablespaceMapping is the repeatable --tablespace-mapping
	// flag value (each "OLDDIR=NEWDIR"). Parsed via
	// restore.ParseTablespaceRemap; refused at the usage layer
	// for malformed entries (non-absolute paths, missing
	// separator, duplicate OLDDIR). Empty / nil = no remap.
	tablespaceMapping []string

	// skipGapCheck bypasses the+ WAL-gap pre-flight when
	// the operator has validated restore safety some other way
	// (recovery drill, manual WAL splice). The override is
	// audit-logged via the wal_gap_check_skipped event so
	// post-incident review sees the choice.
	skipGapCheck bool

	// requireAttestation, when non-empty, gates the restore on a
	// k-of-n threshold attestation existing under the named roster
	// for this manifest body.  Wires through to attestgate.Verify
	// after the manifest read + signature verification but before
	// any write to the target dir.
	requireAttestation string

	// chainStagingRoot pins the directory the chain-restore path
	// uses to materialise links before merging via
	// pg_combinebackup.  Default: a derived path under
	// os.TempDir.  An audit — staging is preserved across
	// retries so a re-run after pg_combinebackup failure (or a
	// mid-link crash) skips already-materialised links.
	chainStagingRoot string

	// resetChainStaging forces a fresh chain-restore staging
	// directory, removing any persisted staging from a previous
	// attempt.  Use when the prior attempt crashed in a way that
	// left staging in an unknown state.
	resetChainStaging bool

	// kmsConfig carries per-call configuration for the cloud KMS
	// provider when restoring a backup wrapped with a cloud KMS KEK
	// (endpoint / region / credentials overrides). Mirrors backup's
	// --kms-config; empty relies on the provider's ambient credentials
	// (e.g. the AWS SDK's env / instance-profile chain, region from the
	// ARN). Ignored for local-custody and unencrypted backups.
	kmsConfig map[string]string

	// Control-plane dispatch flags. When dispatch.controlPlane is
	// set, the CLI POSTs the restore to that URL and polls for
	// completion instead of running it in-process. Useful when the
	// agent that should run the restore lives on a different host
	// than the operator (3am restore from a laptop, K8s in-cluster
	// restore, etc.).
	dispatch dispatchAuthFlags
}

func runRestore(cmd *cobra.Command, opts restoreOpts) error {
	d := DispatcherFrom(cmd)

	// Validate PG-typed PITR target flags up front so a bad value is
	// caught before either the local restore planner or the control-
	// plane round-trip — issue #78 (a typo'd --to-lsn was silently
	// accepted and printed by --preview, then failed only when PG
	// started recovery on the remote agent).
	if err := validateRestoreTargets(&opts); err != nil {
		return err
	}

	// Control-plane mode short-circuits the local execution path.
	// The repo / keyring are the agent's concern; the operator only
	// needs the deployment + target + (optional) PITR target.
	if opts.dispatch.controlPlane != "" {
		return runRestoreControlPlane(cmd, opts)
	}

	// Resolve --repo from the named deployment in config when omitted
	// (explicit flag wins); --target is restore-specific and not stored
	// in config. Local mode only — control-plane dispatch returned above (#12).
	_, opts.repoURL = deploymentDefaults(opts.deployment, "", opts.repoURL)
	var missing []string
	if opts.repoURL == "" {
		missing = append(missing, "--repo")
	}
	if f := cmd.Flags().Lookup("target"); f == nil || f.Value.String() == "" {
		missing = append(missing, "--target")
	}
	if len(missing) > 0 {
		return missingFlagErr(cmd, missing...)
	}

	verifyMode, err := restore.ParseVerifyMode(opts.verifyMode)
	if err != nil {
		return err
	}

	// Resolve the keyring path the same way the backup command does.
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return output.NewError("internal", err.Error()).Wrap(err)
	}
	_, verifier, err := keystore.LoadOrGenerate(p.Keyring.Value)
	if err != nil {
		return output.NewError("internal",
			fmt.Sprintf("restore: signing key: %v", err)).Wrap(err)
	}

	// Resolve `latest` to a concrete backup ID. With
	// --to <time> set, the right seed isn't necessarily the
	// most-recent backup — it's the most-recent backup whose
	// StoppedAt ≤ target_time. PG's recovery replays forward
	// from the seed; a backup taken AFTER target can't be the
	// seed (recovery would have to go backwards). Auto-resolve
	// to the right seed; surface the resolution in the result
	// body so the operator sees what happened.
	//
	// When the operator passed an explicit backup-id, we leave
	// the choice alone but still validate stop_time ≤ target
	// later (validateBackupSeedsTime). An explicit ID with a
	// post-target stop_time is rejected up-front so we don't
	// fail mid-restore.
	// Parse the --to time target ONCE, here, and reuse the result for
	// both seed resolution (below) and the armed recovery_target_time
	// (buildRecovery). Parsing it twice with separate references was a
	// PITR-correctness bug: naturaltime interprets bare-clock
	// "today/yesterday HH:MM" in the reference's zone (its documented
	// "what humans mean" local semantics), so a UTC reference here and
	// a local reference in buildRecovery selected the seed backup for a
	// DIFFERENT instant than the target written to auto.conf on any
	// non-UTC host — off by the local offset, and possibly a different
	// calendar day near midnight. One parse, one instant, no drift.
	var targetTime time.Time
	if opts.toTime != "" {
		var perr error
		targetTime, perr = naturaltime.Parse(opts.toTime, time.Now())
		if perr != nil {
			return output.NewError("usage.bad_time",
				fmt.Sprintf("restore: --to %q: %v", opts.toTime, perr)).Wrap(output.ErrUsage)
		}
	}

	backupID := opts.backupID
	autoResolved := false
	var resolvedFrom string
	if backupID == LatestKeyword {
		// Time-targeted PITR: prefer the time-aware resolver
		// over the unconstrained latest. The seed must be the
		// most-recent backup whose stop_time ≤ the SAME target
		// instant buildRecovery arms below.
		if opts.toTime != "" {
			resolved, err := resolveBackupForTimeFromRepo(cmd.Context(), opts.repoURL, opts.deployment, targetTime, verifier)
			if err != nil {
				return err
			}
			backupID = resolved
			autoResolved = true
			resolvedFrom = "time"
		} else {
			resolved, err := resolveLatestFromRepo(cmd.Context(), opts.repoURL, opts.deployment, verifier)
			if err != nil {
				return err
			}
			backupID = resolved
			autoResolved = true
			resolvedFrom = "latest"
		}
	} else if opts.toTime != "" {
		// Operator passed an explicit backup-id AND a
		// time target. Validate that the chosen backup
		// can actually seed the requested rewind. Reject
		// up-front rather than failing mid-restore.
		if err := validateExplicitBackupForTime(cmd.Context(), opts.repoURL, opts.deployment, backupID, targetTime, verifier); err != nil {
			return err
		}
	}

	// Build the optional Recovery block from PITR flags. nil when
	// none of --to / --to-lsn / --to-name was set; the restore is
	// then a plain non-PITR restore (no recovery.signal written).
	// Built BEFORE the --preview branch so a preview surfaces the
	// target in its body and runs the same reachability gate as a
	// real restore (issue #99). targetTime is the already-parsed --to
	// instant (zero when --to was not set) so buildRecovery does not
	// re-parse and cannot drift from the seed resolution above.
	recovery, err := buildRecovery(opts, targetTime)
	if err != nil {
		return err
	}

	// --preview: dry-run via restore.Preview, return the Plan as the Result body.
	if opts.preview {
		plan, err := restore.Preview(cmd.Context(), restore.PlanOptions{
			RepoURL:    opts.repoURL,
			Deployment: opts.deployment,
			BackupID:   backupID,
			TargetDir:  opts.targetDir,
			Verifier:   verifier,
			Recovery:   recovery,
		})
		if err != nil {
			return err
		}
		return d.Result(output.NewResult(cmd.CommandPath()).WithBody(planBody{Plan: plan}))
	}

	// Parse --tablespace-mapping at the usage layer so a typo'd
	// entry surfaces before any storage round-trip. The
	// resulting TablespaceRemap is empty when the operator
	// passed no flags.
	tsRemap, err := restore.ParseTablespaceRemap(opts.tablespaceMapping)
	if err != nil {
		return output.NewError("usage.bad_tablespace_mapping",
			fmt.Sprintf("restore: %v", err)).Wrap(output.ErrUsage)
	}

	// Threshold-attestation gate.  When --require-threshold-attestation
	// is set, refuse to restore unless a k-of-n attestation under the
	// named roster exists for THIS manifest body.  Defence-in-depth on
	// top of the manifest's own ed25519 signature: the manifest is
	// authentic, and ALSO blessed by k operators.
	if opts.requireAttestation != "" {
		if err := preflightAttestationGate(cmd.Context(),
			opts.repoURL, opts.deployment, backupID, verifier,
			opts.requireAttestation); err != nil {
			return err
		}
	}

	// Wire OnEvent to the dispatcher; suppress in JSON mode so the
	// final Result is the only document in the stream.
	suppressEvents := d.Renderer().Name() == "json"
	res, err := restore.Restore(cmd.Context(), restore.Options{
		RepoURL:             opts.repoURL,
		Deployment:          opts.deployment,
		BackupID:            backupID,
		TargetDir:           opts.targetDir,
		Verifier:            verifier,
		AllowOverwrite:      opts.force,
		AllowForeignCluster: opts.forceForeign,
		Recovery:            recovery,
		TablespaceRemap:     tsRemap,
		ChainStagingRoot:    opts.chainStagingRoot,
		ResetChainStaging:   opts.resetChainStaging,
		VerifyMode:          opts.verifyRestoreMode,
		// Always wire the KEK resolver. It's a no-op for unencrypted
		// backups (Restore only consults it when manifest.Encryption
		// is non-nil) and the right resolver for encrypted ones.
		KEKForRef: keystore.KEKResolver(p.Keyring.Value),
		// Cloud-KMS-encrypted backups unwrap the DEK server-side (the KEK
		// never leaves the HSM). keystore.UnwrapDEK dispatches by scheme;
		// the restore engine only calls this for a cloud KEKRef (issue #102).
		UnwrapDEK: func(ctx context.Context, kekRef string, wrapped []byte) ([]byte, error) {
			return keystore.UnwrapDEK(ctx, kekRef, wrapped, keystore.UnwrapOpts{
				KeyringDir:     p.Keyring.Value,
				ProviderConfig: stringMapToAny(opts.kmsConfig),
			})
		},
		OnEvent: func(e *output.Event) {
			if suppressEvents {
				return
			}
			_ = d.Event(cmd.Context(), e)
		},
	})
	if err != nil {
		return err
	}

	// Advise on the restore_command's runtime dependency: PG recovery
	// shells out to the pg_hardstorage binary to fetch WAL, and that
	// binary must exist in the environment where PG runs recovery — not
	// just where the restore ran. Booting the restored data dir in a
	// vanilla image (e.g. postgres:NN) FATALs with "could not restore
	// file ... from archive: command not found" (issue #107). Suppressed
	// in JSON mode to keep that contract stable.
	if !suppressEvents {
		_ = d.Event(cmd.Context(), output.NewEvent(output.SeverityNotice, "restore", "recovery_command_armed").
			WithBody(map[string]any{
				"message": "PostgreSQL recovery will fetch WAL by running the pg_hardstorage binary via the restore_command in postgresql.auto.conf. That binary must be present where PostgreSQL runs recovery. If PG runs in a separate or vanilla image (e.g. postgres:18), install or mount pg_hardstorage there, or set " + walfetchcmd.RestoreBinEnv + " to its path in that environment before restoring.",
			}))

		// Plain restore (no PITR target) recovers only to the backup's own
		// consistency point (recovery_target='immediate'): it reflects the
		// cluster AS OF the backup, NOT the latest state. Anything created
		// AFTER this backup — a new database, table, or rows — lives only in
		// later WAL and will NOT appear (issue #109). Tell the operator how to
		// get it so a "missing data" surprise becomes an informed choice.
		if recovery == nil {
			_ = d.Event(cmd.Context(), output.NewEvent(output.SeverityNotice, "restore", "recovers_to_backup_point").
				WithBody(map[string]any{
					"message": "this is a plain restore: the cluster is recovered to the moment backup " + res.BackupID + " was taken (recovery_target='immediate'), not to the latest state. Data created AFTER this backup (e.g. a database added later) is in subsequent WAL and will NOT be present. To recover past this point, use point-in-time recovery (--to / --to-lsn to a moment after the change) if WAL is archived, or restore a newer backup.",
				}))
		}
	}

	// Post-restore verification gate.
	verify, verifyErr := restore.Verify(cmd.Context(), opts.targetDir, verifyMode)
	// Even on require-failure we want to attach the VerifyResult to
	// the output so the user sees what happened. Render then return.
	body := restoreResultBody{
		BackupID:          res.BackupID,
		Deployment:        res.Deployment,
		TargetDir:         res.TargetDir,
		FileCount:         res.FileCount,
		BytesWritten:      res.BytesWritten,
		ChunksFetched:     res.ChunksFetched,
		BackupLabelSize:   res.BackupLabelSize,
		TablespaceMapSize: res.TablespaceMapSize,
		DurationMS:        res.Duration.Milliseconds(),
		Verify:            verify,
		Recovery:          recoveryResultFromOpts(recovery),
	}
	if autoResolved {
		body.AutoResolved = true
		body.ResolvedFrom = resolvedFrom
	}
	// Surface tablespace remap in the result body for
	// forensics. Only populated when the operator passed
	// --tablespace-mapping; omitempty preserves the default
	// body shape for the+ JSON-compat regression.
	if !tsRemap.Empty() {
		rows := make([]tablespaceRemapRow, 0, len(tsRemap))
		for _, e := range tsRemap {
			rows = append(rows, tablespaceRemapRow{Old: e.Old, New: e.New})
		}
		body.TablespaceRemap = rows
	}
	if err := d.Result(output.NewResult(cmd.CommandPath()).WithBody(body)); err != nil {
		return err
	}
	return verifyErr
}

// buildRecovery translates the CLI's --to / --to-lsn / --to-name
// (etc.) flags into a *restore.Recovery, or nil if no PITR target
// was requested. Enforces the "at most one of LSN/time/name" rule
// at the CLI layer with a clear structured error before the deeper
// restore.WriteRecoveryFiles repeats the check.
//
// We always emit a restore_command pointing at this binary so PG
// can fetch WAL during recovery — even when no explicit target is
// set (the operator may want plain end-of-WAL recovery from our
// archive).
// targetTime is the pre-parsed --to instant (the caller parses it
// once and threads it here so the recovery_target_time armed on disk
// is the exact instant used to resolve the seed backup). It is the
// zero Time when --to was not set.
func buildRecovery(opts restoreOpts, targetTime time.Time) (*restore.Recovery, error) {
	hasLSN := opts.toLSN != ""
	hasTime := opts.toTime != ""
	hasName := opts.toName != ""

	// No PITR flags at all: plain restore, no recovery files.
	if !hasLSN && !hasTime && !hasName {
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
		return nil, output.NewError("usage.conflicting_targets",
			"restore: at most one of --to, --to-lsn, --to-name may be set").Wrap(output.ErrUsage)
	}

	r := &restore.Recovery{
		Enable:       true,
		Inclusive:    !opts.toExclusive,
		Action:       opts.toAction,
		Timeline:     opts.toTimeline,
		SkipGapCheck: opts.skipGapCheck,
	}

	switch {
	case hasLSN:
		r.TargetLSN = opts.toLSN
	case hasTime:
		// Already parsed by the caller (runRestore) so the seed backup
		// and this armed target share one instant — see the targetTime
		// comment there.
		r.TargetTime = targetTime
	case hasName:
		r.TargetName = opts.toName
	}

	cmd, err := buildRestoreCommandString(opts.deployment, opts.repoURL)
	if err != nil {
		return nil, err
	}
	r.RestoreCommand = cmd
	return r, nil
}

// buildRestoreCommandString assembles the literal `restore_command`
// GUC value PG will execute for each WAL segment during recovery.
//
// The path of the running binary is used as the command — this
// works for the common case where the agent restoring the cluster is
// also the agent that PG will call back. For Patroni / k8s scenarios
// where PG recovers in a different environment (a vanilla postgres
// image, another host), the operator sets walfetchcmd.RestoreBinEnv
// (PG_HARDSTORAGE_RESTORE_BIN) to the binary's path in that
// environment; walfetchcmd.Build honours it (issue #107).
func buildRestoreCommandString(deployment, repoURL string) (string, error) {
	bin, err := os.Executable()
	if err != nil {
		return "", output.NewError("internal",
			fmt.Sprintf("restore: locate own binary for restore_command: %v", err)).Wrap(err)
	}
	// PG substitutes %f with the wanted segment file name and %p with
	// the destination path. The shim's positional args mirror this.
	// Goes through walfetchcmd.Build so the exit-6 → exit-1 wrapper
	// is applied — see that package's docstring for the full
	// rationale (restore sandbox recovery loop).
	return walfetchcmd.Build(bin, deployment, repoURL), nil
}

// validateRestoreTargets parses and normalises the PG-typed PITR
// flags (--to-lsn, --to-timeline, --to-action) so the CLI rejects
// malformed input with a clear usage error before reaching either
// the local planner or the control-plane round-trip.
//
// On success --to-lsn is rewritten to the canonical "X/Y" form
// returned by pglogrepl.LSN.String so downstream code (preview,
// auto.conf emit, control-plane JSON body) sees a single uniform
// shape regardless of the operator's casing.
func validateRestoreTargets(opts *restoreOpts) error {
	if opts.toLSN != "" {
		// pglogrepl.ParseLSN tolerates trailing garbage and PG accepts
		// non-canonical leading zeros, so a strict "<hex>/<hex>" shape
		// check goes ahead of the parser. Without it "0/3000028x"
		// would be silently truncated to a valid uint64.
		if !restore.LooksLikeLSN(opts.toLSN) {
			return output.NewError("usage.bad_lsn",
				fmt.Sprintf("restore: --to-lsn %q: expected hex form like 0/3000028",
					opts.toLSN)).Wrap(output.ErrUsage)
		}
		lsn, err := pglogrepl.ParseLSN(opts.toLSN)
		if err != nil {
			return output.NewError("usage.bad_lsn",
				fmt.Sprintf("restore: --to-lsn %q: %v (expected hex form like 0/3000028)",
					opts.toLSN, err)).Wrap(output.ErrUsage)
		}
		opts.toLSN = lsn.String()
	}
	if opts.toTimeline != "" && opts.toTimeline != "latest" {
		n, err := strconv.ParseUint(opts.toTimeline, 10, 32)
		if err != nil || n == 0 {
			return output.NewError("usage.bad_timeline",
				fmt.Sprintf("restore: --to-timeline %q: must be \"latest\" or a positive integer",
					opts.toTimeline)).Wrap(output.ErrUsage)
		}
	}
	switch opts.toAction {
	case "", "pause", "promote", "shutdown":
		// ok
	default:
		return output.NewError("usage.bad_action",
			fmt.Sprintf("restore: --to-action %q: must be one of pause|promote|shutdown",
				opts.toAction)).Wrap(output.ErrUsage)
	}
	return nil
}

// resolveLatestFromRepo opens the repo's storage layer and calls
// restore.ResolveLatest, mapping errors to structured output errors
// the CLI consumer can act on.
func resolveLatestFromRepo(ctx context.Context, repoURL, deployment string, verifier *backup.Verifier) (string, error) {
	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		if errors.Is(err, repo.ErrNotARepo) {
			return "", output.NewError("notfound.repo",
				fmt.Sprintf("restore: no pg_hardstorage repository at %s", repoURL)).Wrap(err)
		}
		if errors.Is(err, storage.ErrUnknownScheme) {
			return "", output.NewError("usage.unknown_scheme", err.Error()).Wrap(output.ErrUsage)
		}
		return "", fmt.Errorf("restore: open repo: %w", err)
	}
	defer sp.Close()

	id, err := restore.ResolveLatest(ctx, sp, deployment, verifier)
	if err != nil {
		if errors.Is(err, restore.ErrNoBackupsFound) {
			return "", restore.FormatNoBackupsError(deployment)
		}
		return "", fmt.Errorf("restore: resolve latest: %w", err)
	}
	return id, nil
}

// resolveBackupForTimeFromRepo opens the repo's storage layer
// and calls restore.ResolveBackupForTime. Mirror of
// resolveLatestFromRepo with structured error mapping for the
// time-target case (no backup before target → structured
// notfound.backup_before_time + Suggestion).
func resolveBackupForTimeFromRepo(ctx context.Context, repoURL, deployment string, target time.Time, verifier *backup.Verifier) (string, error) {
	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		if errors.Is(err, repo.ErrNotARepo) {
			return "", output.NewError("notfound.repo",
				fmt.Sprintf("restore: no pg_hardstorage repository at %s", repoURL)).Wrap(err)
		}
		if errors.Is(err, storage.ErrUnknownScheme) {
			return "", output.NewError("usage.unknown_scheme", err.Error()).Wrap(output.ErrUsage)
		}
		return "", fmt.Errorf("restore: open repo: %w", err)
	}
	defer sp.Close()

	id, err := restore.ResolveBackupForTime(ctx, sp, deployment, target, verifier)
	if err == nil {
		return id, nil
	}
	if errors.Is(err, restore.ErrNoBackupsFound) {
		return "", restore.FormatNoBackupsError(deployment)
	}
	var noTime *restore.NoBackupBeforeTimeError
	if errors.As(err, &noTime) {
		return "", restore.FormatNoBackupBeforeTimeError(noTime)
	}
	return "", fmt.Errorf("restore: resolve backup for target time: %w", err)
}

// validateExplicitBackupForTime guards against the "operator
// passed an explicit backup-id whose stop_time is AFTER the
// --to target" misuse. PG can't replay backwards from a future
// backup; without this check, the restore would fail at
// recovery time with a confusing "WAL not found" — better to
// reject up-front with the explicit explanation.
//
// No-op when target is zero (no time-target) or backupID is
// the latest keyword (already handled by the auto-resolve).
func validateExplicitBackupForTime(ctx context.Context, repoURL, deployment, backupID string, target time.Time, verifier *backup.Verifier) error {
	if target.IsZero() {
		return nil
	}
	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		// Earlier resolution already opened the repo; if it
		// has gone away between then and now, surface a clean
		// error.
		return fmt.Errorf("restore: re-open repo for time validation: %w", err)
	}
	defer sp.Close()
	store := backup.NewManifestStore(sp)
	m, rerr := store.Read(ctx, deployment, backupID, verifier)
	if rerr != nil {
		// If we can't read the manifest, the actual restore
		// will surface that error. Don't fabricate a different
		// one here.
		return nil
	}
	if m.StoppedAt.After(target) {
		return output.NewError("conflict.backup_after_target",
			fmt.Sprintf("restore: backup %s/%s stopped at %s, AFTER --to target %s; PITR cannot replay backwards from a future backup",
				deployment, backupID,
				m.StoppedAt.UTC().Format(time.RFC3339),
				target.UTC().Format(time.RFC3339))).
			WithSuggestion(&output.Suggestion{
				Human: "either pick an earlier backup (the auto-resolve via --to with `latest` does this for you), or move --to forward to be at or after the backup's stop_time.",
			})
	}
	return nil
}

// restoreResultBody is the typed body for `restore`'s success Result.
//
// AutoResolved + ResolvedFrom surface only when the operator
// passed `latest` as the backup-id and the CLI auto-picked a
// concrete backup. ResolvedFrom is "time" when the resolver
// honoured --to (most-recent backup with stop_time ≤ target)
// and "latest" when it just picked the most-recent backup.
// Both fields are omitempty; default body for an explicit
// backup-id stays byte-identical to+.
type restoreResultBody struct {
	BackupID          string                `json:"backup_id"`
	Deployment        string                `json:"deployment"`
	TargetDir         string                `json:"target_dir"`
	FileCount         int                   `json:"file_count"`
	BytesWritten      int64                 `json:"bytes_written"`
	ChunksFetched     int                   `json:"chunks_fetched"`
	BackupLabelSize   int                   `json:"backup_label_size"`
	TablespaceMapSize int                   `json:"tablespace_map_size"`
	DurationMS        int64                 `json:"duration_ms"`
	Verify            *restore.VerifyResult `json:"verify,omitempty"`
	Recovery          *recoveryArmed        `json:"recovery,omitempty"`
	AutoResolved      bool                  `json:"auto_resolved,omitempty"`
	ResolvedFrom      string                `json:"resolved_from,omitempty"` // "time" | "latest"
	// TablespaceRemap surfaces the operator-supplied path
	// redirects when --tablespace-mapping was used. Empty /
	// nil = no remap requested; the field is omitempty so the
	// default body shape stays byte-identical to+
	// (24-month JSON-compat).
	TablespaceRemap []tablespaceRemapRow `json:"tablespace_remap,omitempty"`
}

// tablespaceRemapRow surfaces one OLD→NEW path remap in the
// result body. Mirrors restore.TablespaceRemapEntry for JSON
// rendering with field names that match the operator's flag
// shape.
type tablespaceRemapRow struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// recoveryArmed is what we report back to the user about the PITR
// configuration applied to the restored data dir. It's a flat read-
// only view of the restore.Recovery the runtime received — never
// mirrors the literal restore_command (which contains a full path)
// because that's a system-internal detail and noise in the operator
// view.
type recoveryArmed struct {
	TargetLSN  string `json:"target_lsn,omitempty"`
	TargetTime string `json:"target_time,omitempty"`
	TargetName string `json:"target_name,omitempty"`
	Inclusive  bool   `json:"inclusive"`
	Action     string `json:"action"`
	Timeline   string `json:"timeline"`
}

// recoveryResultFromOpts mirrors a restore.Recovery into the result-
// shaped recoveryArmed. nil in, nil out.
func recoveryResultFromOpts(r *restore.Recovery) *recoveryArmed {
	if r == nil || !r.Enable {
		return nil
	}
	out := &recoveryArmed{
		TargetLSN:  r.TargetLSN,
		TargetName: r.TargetName,
		Inclusive:  r.Inclusive,
		Action:     r.Action,
		Timeline:   r.Timeline,
	}
	if !r.TargetTime.IsZero() {
		out.TargetTime = r.TargetTime.UTC().Format(time.RFC3339)
	}
	if out.Action == "" {
		out.Action = "pause"
	}
	if out.Timeline == "" {
		out.Timeline = "latest"
	}
	return out
}

// planBody is the typed body for `restore --preview`.
type planBody struct {
	*restore.Plan
}

// WriteText is the text-renderer hook. Mirrors the backup command's
// shape so the two commands feel like a matched pair.
func (b restoreResultBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintln(bw, "✓ Restore complete")
	fmt.Fprintf(bw, "  Backup:        %s\n", b.BackupID)
	fmt.Fprintf(bw, "  Deployment:    %s\n", b.Deployment)
	fmt.Fprintf(bw, "  Target:        %s\n", b.TargetDir)
	fmt.Fprintf(bw, "  Files:         %d\n", b.FileCount)
	fmt.Fprintf(bw, "  Bytes written: %s\n", humanBytes(b.BytesWritten))
	fmt.Fprintf(bw, "  Chunks:        %d\n", b.ChunksFetched)
	if b.BackupLabelSize > 0 {
		fmt.Fprintf(bw, "  backup_label:  %d bytes\n", b.BackupLabelSize)
	}
	if b.TablespaceMapSize > 0 {
		fmt.Fprintf(bw, "  tablespace_map: %d bytes\n", b.TablespaceMapSize)
	}
	fmt.Fprintf(bw, "  Duration:      %d ms", b.DurationMS)
	if b.Verify != nil {
		fmt.Fprintf(bw, "\n  Verification:  %s", b.Verify.Status)
		if b.Verify.Status == "failed" && b.Verify.ExitCode != 0 {
			fmt.Fprintf(bw, " (exit %d)", b.Verify.ExitCode)
		}
		if b.Verify.Status == "missing_tool" {
			fmt.Fprintf(bw, " (pg_verifybackup not on PATH)")
		}
	}
	if b.Recovery != nil {
		fmt.Fprintln(bw, "\n  Recovery armed:")
		switch {
		case b.Recovery.TargetLSN != "":
			fmt.Fprintf(bw, "    Stop at LSN:  %s\n", b.Recovery.TargetLSN)
		case b.Recovery.TargetTime != "":
			fmt.Fprintf(bw, "    Stop at time: %s\n", b.Recovery.TargetTime)
		case b.Recovery.TargetName != "":
			fmt.Fprintf(bw, "    Stop at name: %s\n", b.Recovery.TargetName)
		default:
			fmt.Fprintln(bw, "    Stop at:      end of available WAL")
		}
		fmt.Fprintf(bw, "    Action:       %s\n", b.Recovery.Action)
		fmt.Fprintf(bw, "    Timeline:     %s\n", b.Recovery.Timeline)
		fmt.Fprintf(bw, "    Inclusive:    %t", b.Recovery.Inclusive)
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

// WriteText is the text-renderer hook for `restore --preview`.
func (b planBody) WriteText(w io.Writer) error {
	if b.Plan == nil {
		return nil
	}
	p := b.Plan
	bw := &strings.Builder{}
	fmt.Fprintln(bw, "Restore plan (preview only — no files written)")
	fmt.Fprintf(bw, "  Backup:           %s\n", p.BackupID)
	fmt.Fprintf(bw, "  Deployment:       %s\n", p.Deployment)
	fmt.Fprintf(bw, "  Target:           %s\n", p.TargetDir)
	fmt.Fprintf(bw, "  PostgreSQL:       %d\n", p.PGVersion)
	fmt.Fprintf(bw, "  Cluster ID:       %s\n", p.SystemIdentifier)
	fmt.Fprintf(bw, "  Backup stop LSN:  %s (TLI %d)\n", p.StopLSN, p.Timeline)
	// Surface the PITR target when the operator set one (issue
	// #99: before this block, --to-lsn produced no visible
	// change in the preview output because the planner ignored
	// it; now the preview echoes the target back so the
	// operator can confirm the flag took effect).
	if p.Recovery != nil {
		switch {
		case p.Recovery.TargetLSN != "":
			fmt.Fprintf(bw, "  Recovery target:  LSN %s (inclusive=%t)\n",
				p.Recovery.TargetLSN, p.Recovery.Inclusive)
		case p.Recovery.TargetTime != "":
			fmt.Fprintf(bw, "  Recovery target:  time %s (inclusive=%t)\n",
				p.Recovery.TargetTime, p.Recovery.Inclusive)
		case p.Recovery.TargetName != "":
			fmt.Fprintf(bw, "  Recovery target:  name %q (inclusive=%t)\n",
				p.Recovery.TargetName, p.Recovery.Inclusive)
		}
		if p.Recovery.Action != "" {
			fmt.Fprintf(bw, "  On target reached:%s\n", " "+p.Recovery.Action)
		}
		if p.Recovery.Timeline != "" {
			fmt.Fprintf(bw, "  Recovery TLI:     %s\n", p.Recovery.Timeline)
		}
	}
	fmt.Fprintf(bw, "  Files:            %d\n", p.FileCount)
	fmt.Fprintf(bw, "  Total bytes:      %s\n", humanBytes(p.TotalBytes))
	fmt.Fprintf(bw, "  Chunk refs:       %d (%d unique, %s after dedup)\n",
		p.ChunkRefCount, p.UniqueChunkCount, humanBytes(p.UniqueChunkBytes))
	if p.BackupLabelSize > 0 {
		fmt.Fprintf(bw, "  backup_label:     %d bytes\n", p.BackupLabelSize)
	}
	if p.TablespaceMapSize > 0 {
		fmt.Fprintf(bw, "  tablespace_map:   %d bytes\n", p.TablespaceMapSize)
	}
	fmt.Fprintf(bw, "  Estimated RTO:    %d ms (assuming %s/s)\n",
		p.EstimatedRTO.Milliseconds(), humanBytes(p.AssumedThroughput))
	if p.WALArchiveHoleLSN != "" {
		fmt.Fprintf(bw, "  WAL archive hole: ✗ segment missing at %s — recovery would HALT before the target\n", p.WALArchiveHoleLSN)
		fmt.Fprintf(bw, "                    (inspect with `pg_hardstorage wal list %s`; restore from a later backup or pick a target before the hole)\n", p.Deployment)
	}
	switch {
	case p.WALArchiveHoleLSN != "":
		fmt.Fprintf(bw, "  Pre-flight:       ✗ WAL archive hole (target unreachable)")
	case p.PreflightOK:
		fmt.Fprintf(bw, "  Pre-flight:       ✓ ready")
	default:
		fmt.Fprintf(bw, "  Pre-flight:       ✗ %d issue(s)\n", len(p.PreflightIssues))
		for _, issue := range p.PreflightIssues {
			fmt.Fprintf(bw, "                    - %s\n", issue)
		}
		fmt.Fprintf(bw, "  (run with --force to overwrite a non-empty target)")
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

// preflightAttestationGate enforces the --require-threshold-attestation
// gate before any disk write happens.  Reads the manifest, runs the
// attestgate check, and returns a structured error on refusal.  The
// error codes route to standard exit codes:
//
//   - notfound.attestation       → exit 6 (no attestation found)
//   - verify.attestation_quorum  → exit 9 (quorum not met)
//   - verify.attestation_subject → exit 9 (subject mismatch — attestation
//     pins a different manifest body)
//   - verify.attestation_roster  → exit 9 (attestation references a
//     different roster)
//
// Bare reads of the repo / manifest pass through their normal error
// channels.
func preflightAttestationGate(
	ctx context.Context,
	repoURL, deployment, backupID string,
	verifier *backup.Verifier,
	rosterID string,
) error {
	if rosterID == "" {
		return nil
	}
	_, sp, err := openRepo(ctx, repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	store := backup.NewManifestStore(sp)
	m, err := store.Read(ctx, deployment, backupID, verifier)
	if err != nil {
		return err
	}
	if err := attestgate.Verify(ctx, sp, m, attestgate.Options{
		RosterID: rosterID,
		// Anchor the roster's creator to the operator keyring — the same
		// key that verifies the manifest above. Without this a repo-write
		// attacker could plant a self-signed 1-of-1 roster that satisfies
		// its own quorum and waves the restore through.
		TrustedKeys: []ed25519.PublicKey{verifier.PublicKey()},
	}); err != nil {
		switch {
		case errors.Is(err, attestgate.ErrNoTrustAnchor):
			return output.NewError("verify.attestation_trust_anchor",
				fmt.Sprintf("restore: %v", err)).Wrap(err)
		case errors.Is(err, attestgate.ErrRosterUntrusted):
			return output.NewError("verify.attestation_roster_untrusted",
				fmt.Sprintf("restore: %v — the roster was not created by this operator's key; a forged roster cannot gate a restore",
					err)).Wrap(err)
		case errors.Is(err, attestgate.ErrAttestationMissing):
			return output.NewError("notfound.attestation",
				fmt.Sprintf("restore: %v", err)).
				WithSuggestion(&output.Suggestion{
					Human: "collect threshold attestations from k of n roster members before restoring",
					Command: fmt.Sprintf(
						"pg_hardstorage threshold attest sign backup_manifest %s --hash <manifest-canonical-hash> --roster %s",
						backupID, rosterID),
				}).Wrap(err)
		case errors.Is(err, attestgate.ErrQuorumNotMet):
			return output.NewError("verify.attestation_quorum",
				fmt.Sprintf("restore: %v", err)).Wrap(err)
		case errors.Is(err, attestgate.ErrSubjectHashMismatch):
			return output.NewError("verify.attestation_subject",
				fmt.Sprintf("restore: %v — the attestation was likely signed for a different manifest version (KEK rotation, re-encryption); collect fresh signatures",
					err)).Wrap(err)
		case errors.Is(err, attestgate.ErrRosterMismatch):
			return output.NewError("verify.attestation_roster",
				fmt.Sprintf("restore: %v", err)).Wrap(err)
		}
		return output.NewError("verify.attestation_invalid",
			fmt.Sprintf("restore: %v", err)).Wrap(err)
	}
	return nil
}
