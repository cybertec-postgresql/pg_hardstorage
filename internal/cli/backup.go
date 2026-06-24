// backup.go — 'backup' CLI verb: takes a backup of a named deployment.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/tarsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/capacity"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// newRealBackupCmd is the in-development real backup command.
func newRealBackupCmd() *cobra.Command {
	var opts runOptions
	c := &cobra.Command{
		Use:   "backup <deployment>",
		Short: "Take a backup of a deployment",
		Long: `Run a full base backup of a PostgreSQL deployment.

The signing keypair is loaded (or generated on first run) from the
resolved keyring directory; see ` + "`" + `pg_hardstorage doctor` + "`" + ` for the path.

Encryption is chosen automatically: if a KEK file (kek.bin) exists
in the keyring directory, every chunk is AES-256-GCM-encrypted and
the wrapped DEK lands in the manifest. Pass --no-encrypt to force
an unencrypted backup even when a KEK is present (escape hatch for
operators with mixed posture); pass --encrypt to require encryption
and error when no KEK is found.

The repository must already exist — create it with ` + "`" + `pg_hardstorage repo init` + "`" + `.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.deployment = args[0]
			return runBackup(cmd, opts)
		},
	}
	c.Flags().StringVar(&opts.pgConn, "pg-connection", "",
		"libpq connection string for the source PostgreSQL (required)")
	c.Flags().StringVar(&opts.repoURL, "repo", "",
		"repository URL (file://, s3://, ...) — must already exist (required)")
	c.Flags().StringVar(&opts.label, "label", "",
		"backup label for backup_label (default: derived from backup ID)")
	c.Flags().StringVar(&opts.tenant, "tenant", "default",
		"tenant scope for the backup")
	c.Flags().BoolVar(&opts.fast, "fast", false,
		"force an immediate CHECKPOINT (faster start, brief I/O burst)")
	c.Flags().BoolVar(&opts.includeWAL, "include-wal", false,
		"embed every WAL segment needed for recovery into the basebackup "+
			"tar stream (equivalent to pg_basebackup -X stream).  Default off — "+
			"production deployments use `wal stream` for continuous archiving.  "+
			"Set this for self-contained correctness scenarios where no separate "+
			"WAL archiver is running.")
	c.Flags().BoolVar(&opts.encrypt, "encrypt", false,
		"require encryption (errors if no KEK file is present)")
	c.Flags().BoolVar(&opts.noEncrypt, "no-encrypt", false,
		"force unencrypted backup even when a KEK file is present")
	c.Flags().StringVar(&opts.kekRef, "kek", "",
		"KEK reference (default: local:default if a kek.bin is present). "+
			"Cloud-KMS schemes: aws-kms://<arn-or-alias-or-key-id>. "+
			"The runner opens the matching kms.Provider and asks it to wrap the per-backup DEK; "+
			"the manifest stamps the KEKRef so restore can pick the right unwrap path.")
	c.Flags().StringToStringVar(&opts.kmsConfig, "kms-config", nil,
		"per-call config for the cloud-KMS provider (e.g. region=us-east-1,use_fips_endpoint=true). "+
			"Only consulted when --kek is a cloud scheme.")
	c.Flags().StringVar(&opts.incrementalFrom, "incremental-from", "",
		"take a PG 17+ incremental backup against this parent backup ID; "+
			"requires summarize_wal=on on the source DB")
	c.Flags().BoolVar(&opts.ignoreCapacity, "ignore-capacity", false,
		"skip the capacity pre-flight (refuse-on-low-free-space gate); use only when the projection is misleading")
	c.Flags().Float64Var(&opts.capacitySafetyFactor, "capacity-safety-factor", capacity.DefaultSafetyFactor,
		"safety multiplier for the capacity pre-flight (default 1.1 = 110% of projected); ignored under --ignore-capacity")
	c.Flags().BoolVar(&opts.allowConcurrent, "allow-concurrent", false,
		"skip the per-deployment backup lease and allow a second backup of the same deployment to run concurrently (doubles load on the source)")
	c.Flags().DurationVar(&opts.stallTimeout, "stall-timeout", 0,
		"abort with backup.io_starved if no progress event for this long (0 = disabled; soak drivers pin 5m)")
	c.Flags().BoolVarP(&opts.verbose, "verbose", "v", false,
		"emit one line per regular file as it commits to the CAS — file path, "+
			"logical size, chunk count, deduped chunks, and bytes the CAS actually "+
			"had to store after dedup.  Useful for live progress on large backups; "+
			"suppressed under --output json (JSON consumers see file events as "+
			"backup.file_archived structured events instead).")
	registerDispatchFlags(c, &opts.dispatch)

	// `backup` doubles as a parent for destructive sub-ops.
	// Cobra routes to `delete` only when the first positional
	// literally matches the subcommand name; `pg_hardstorage backup
	// db1` continues to take a backup as before.
	c.AddCommand(newBackupDeleteCmd())
	c.AddCommand(newBackupUndeleteCmd())
	c.AddCommand(newBackupCompareCmd())
	c.AddCommand(newBackupGraphCmd())
	return c
}

// runOptions is the shape we hand to runBackup, separated so the entry
// can be tested by passing a struct rather than re-parsing flags.
type runOptions struct {
	deployment      string
	pgConn          string
	repoURL         string
	label           string
	fast            bool
	includeWAL      bool
	tenant          string
	encrypt         bool
	noEncrypt       bool
	kekRef          string
	kmsConfig       map[string]string
	incrementalFrom string

	// ignoreCapacity skips the pre-flight free-space gate.
	// Operators with intentionally over-tight repos (or
	// projections known to overstate the actual size) opt
	// out via this flag.
	ignoreCapacity       bool
	capacitySafetyFactor float64

	// verbose toggles per-file progress reporting (issue #9).
	// In text mode the dispatcher renders one line per file
	// (path / size / chunks / dedup); JSON consumers see
	// backup.file_archived structured events instead.
	verbose bool

	// stallTimeout aborts the backup with backup.io_starved
	// when no progress event arrives within this window.
	// Zero (the default) disables the guard, preserving
	// interactive-CLI semantics where Ctrl-C is the operator's
	// own watchdog.  Soak / driver scripts pass --stall-timeout
	// 5m so a wedged backup fails cleanly instead of hanging
	// the orchestrator indefinitely.
	stallTimeout time.Duration

	// allowConcurrent disables the per-deployment backup lease, so a
	// second backup of the same deployment can run alongside one
	// that's already in progress.  Off by default; the operator opts
	// in only when they accept the doubled load on the source.
	allowConcurrent bool

	// Control-plane dispatch flags. When dispatch.controlPlane is
	// set, the CLI POSTs the backup to that URL and polls for
	// completion instead of running it in-process. Mirrors the
	// restore --control-plane mode that landed first; both ride the
	// same DispatchClient.
	dispatch dispatchAuthFlags
}

func runBackup(cmd *cobra.Command, opts runOptions) error {
	d := DispatcherFrom(cmd)

	// Control-plane mode short-circuits the local execution path.
	// The pg-connection / keyring / repo are the agent's concern;
	// the operator only needs the deployment + (optional) backup
	// args.
	if opts.dispatch.controlPlane != "" {
		return runBackupControlPlane(cmd, opts)
	}

	// `backup <deployment>` resolves --pg-connection / --repo from the
	// named deployment in pg_hardstorage.yaml when the operator didn't
	// pass them on the command line; explicit flags still win (#12).
	opts.pgConn, opts.repoURL = deploymentDefaults(opts.deployment, opts.pgConn, opts.repoURL)

	// Required-field checks (local mode only — a control-plane dispatch
	// returned above). Validate the resolved values, not the raw flags:
	// they may have been filled from the deployment config just above.
	var missing []string
	if opts.pgConn == "" {
		missing = append(missing, "--pg-connection")
	}
	if opts.repoURL == "" {
		missing = append(missing, "--repo")
	}
	if len(missing) > 0 {
		return missingFlagErr(cmd, missing...)
	}

	// Resolve the keyring path via the same precedence chain doctor
	// uses (env / config / FHS / XDG). Slice 2's paths.Resolve is the
	// single source of truth.
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return output.NewError("internal", err.Error()).Wrap(err)
	}
	signer, verifier, err := keystore.LoadOrGenerate(p.Keyring.Value)
	if err != nil {
		return output.NewError("internal",
			fmt.Sprintf("backup: signing key: %v", err)).Wrap(err)
	}

	// Resolve the encryption posture from --encrypt / --no-encrypt
	// and from the keyring contents. Auto-detect when neither flag
	// is set: present KEK ⇒ encrypt; absent KEK ⇒ plaintext.
	encConfig, err := resolveBackupEncryption(cmd.Context(), p.Keyring.Value, opts.encrypt, opts.noEncrypt, opts.kekRef, opts.kmsConfig)
	if err != nil {
		return err
	}
	// When the backup uses a cloud-KMS provider, close it on
	// the way out so SDK background goroutines don't leak past
	// the command's lifetime.
	if encConfig != nil && encConfig.Provider != nil {
		defer encConfig.Provider.Close()
	}

	// PG 17+ incremental: load the parent's PG-emitted backup_manifest
	// (which we stamped into our own manifest's BackupLabel field's
	// neighbour at parent-commit time). We surface a structured error
	// when the parent doesn't exist — the operator's first hit on a
	// typo'd parent ID is a clean exit, not a mid-run failure.
	var incrConfig *runner.IncrementalConfig
	if opts.incrementalFrom != "" {
		incrConfig, err = loadIncrementalConfig(cmd.Context(), opts.repoURL, opts.deployment, opts.incrementalFrom, verifier)
		if err != nil {
			return err
		}
	}

	// Capacity pre-flight (closes the SPEC's resilience #11
	// requirement). Default ON; --ignore-capacity opts out.
	// Skipped silently for object-store backends (Unsupported)
	// and for fresh deployments where there's no prior
	// manifest to project from.
	if !opts.ignoreCapacity {
		if err := backupCapacityPreflight(cmd.Context(), opts, verifier); err != nil {
			return err
		}
	}

	// Build the runner options. OnEvent threads progress events through
	// the dispatcher — except in JSON mode, where we suppress them so
	// the consumer sees one Result document per command, not a stream
	// of mixed events + result.
	rendererName := d.Renderer().Name()
	suppressEvents := rendererName == "json"

	// OnFile (issue #9 / `--verbose`) plumbs per-file progress
	// from tarsink → runner → dispatcher.  Always wire when
	// --verbose is set; in JSON mode the dispatcher converts the
	// stats into a structured backup.file_archived event so
	// machine consumers see them too, in text mode it formats a
	// single readable line per file.  When --verbose is off we
	// leave OnFile nil and the tarsink fast-path skips the
	// callback entirely.
	var onFile func(tarsink.FileStats)
	if opts.verbose {
		onFile = func(fs tarsink.FileStats) {
			ev := output.NewEvent(output.SeverityInfo, "backup", "file_archived").
				WithSubject(output.Subject{Deployment: opts.deployment}).
				WithBody(backupFileEventBody{
					Path:          fs.Path,
					Size:          fs.Size,
					ChunkCount:    fs.ChunkCount,
					DedupedChunks: fs.DedupedChunks,
					UniqueBytes:   fs.UniqueBytes,
				})
			_ = d.Event(cmd.Context(), ev)
		}
	}

	res, err := runner.Take(cmd.Context(), runner.TakeOptions{
		PGConnString: opts.pgConn,
		RepoURL:      opts.repoURL,
		Deployment:   opts.deployment,
		Tenant:       opts.tenant,
		Signer:       signer,
		Verifier:     verifier,
		Label:        opts.label,
		Fast:         opts.fast,
		IncludeWAL:   opts.includeWAL,
		Encryption:   encConfig,
		Incremental:  incrConfig,
		StallTimeout: opts.stallTimeout,
		SkipLease:    opts.allowConcurrent,
		OnEvent: func(e *output.Event) {
			if suppressEvents {
				return
			}
			_ = d.Event(cmd.Context(), e)
		},
		OnFile: onFile,
	})
	if err != nil {
		return err
	}

	body := backupResultBody{
		BackupID:         res.BackupID,
		Deployment:       res.Deployment,
		Tenant:           res.Tenant,
		PGVersion:        res.PGVersion,
		SystemIdentifier: res.SystemIdentifier,
		StartLSN:         res.StartLSN,
		StopLSN:          res.StopLSN,
		Timeline:         res.Timeline,
		DurationMS:       res.Duration.Milliseconds(),
		FileCount:        res.FileCount,
		TablespaceCount:  res.TablespaceCount,
		LogicalBytes:     res.LogicalBytes,
		UniqueChunkCount: res.UniqueChunkCount,
		TotalChunkRefs:   res.TotalChunkRefs,
		UniqueChunkBytes: res.UniqueChunkBytes,
		PrimaryKey:       res.PrimaryKey,
		Encrypted:        encConfig != nil,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// loadIncrementalConfig reads the named parent backup's manifest
// from the repo and extracts the PG backup_manifest bytes for
// passing through BASE_BACKUP INCREMENTAL.
//
// Errors map to the structured CLI codes:
//   - parent missing → notfound.backup
//   - parent has no PGBackupManifest → usage.bad_flag
//     with a hint that incrementals require a freshly-taken parent
//   - signature verification failure on the parent → conflict.signature
func loadIncrementalConfig(ctx context.Context, repoURL, deployment, parentID string, verifier *backup.Verifier) (*runner.IncrementalConfig, error) {
	_, sp, err := openRepo(ctx, repoURL)
	if err != nil {
		return nil, err
	}
	defer sp.Close()
	// "latest" is the well-known sentinel for "the most recent
	// live backup of this deployment".  Resolve it here so
	// `--incremental-from latest` works the same way
	// `restore latest` does — this is the contract the pgBackRest
	// shim (compat/pgbackrest/backup.go) relies on when it
	// translates `pgbackrest backup --type=incr` to
	// `--incremental-from latest`, and it matches the operator's
	// expectation that "latest" is a universal stable handle.
	if strings.EqualFold(strings.TrimSpace(parentID), "latest") {
		resolved, err := restore.ResolveLatest(ctx, sp, deployment, verifier)
		if err != nil {
			if errors.Is(err, restore.ErrNoBackupsFound) {
				return nil, output.NewError("notfound.backup",
					fmt.Sprintf("backup --incremental-from=latest: no live backups for deployment %q", deployment)).
					WithSuggestion(&output.Suggestion{
						Human: "take a full backup first to anchor the incremental chain",
					}).Wrap(err)
			}
			return nil, fmt.Errorf("backup --incremental-from=latest: resolve: %w", err)
		}
		parentID = resolved
	}
	store := backup.NewManifestStore(sp)
	parent, err := store.Read(ctx, deployment, parentID, verifier)
	if err != nil {
		return nil, output.NewError("notfound.backup",
			fmt.Sprintf("backup --incremental-from: parent %q not found in repo: %v",
				parentID, err)).
			WithSuggestion(&output.Suggestion{
				Human: "verify the parent backup ID with `pg_hardstorage list <deployment>`",
			}).Wrap(err)
	}
	if len(parent.PGBackupManifest) == 0 {
		return nil, output.NewError("usage.bad_flag",
			fmt.Sprintf("backup --incremental-from: parent %q has no pg_backup_manifest field (pre-backup); take a fresh full backup first to anchor the incremental chain",
				parentID)).
			Wrap(output.ErrUsage)
	}
	return &runner.IncrementalConfig{
		ParentBackupID:   parent.BackupID,
		ParentPGManifest: parent.PGBackupManifest,
	}, nil
}

// Sanity import to keep `repo` referenced (loadIncrementalConfig
// reaches it via openRepo's signature).
var _ = repo.HSREPOFilename

// resolveBackupEncryption decides whether the run encrypts and, if
// so, returns the EncryptionConfig the runner consumes. Posture
// matrix:
//
//	encrypt=true  noEncrypt=true   → ExitMisuse (mutually exclusive)
//	encrypt=true  KEK absent       → ExitError (no key to encrypt with)
//	encrypt=true  KEK present      → encrypt
//	noEncrypt=true                 → plaintext (regardless of KEK)
//	(neither flag) KEK present     → encrypt (auto-on)
//	(neither flag) KEK absent      → plaintext (auto-off)
//
// The auto-on-when-key-present default is the SPEC's "encryption is
// on by default" posture — operators who run init with --encrypt get
// every subsequent backup encrypted without further opt-in.
//
// Cloud-KMS path: when kekRef has a registered scheme prefix
// (e.g. "aws-kms://..."), we open the provider via kms.DefaultRegistry
// and stuff it onto the EncryptionConfig.  The runner branches on
// Provider != nil and uses Provider.WrapDEK instead of the on-disk
// KEK.  No kek.bin is required for the cloud path.
func resolveBackupEncryption(ctx context.Context, keyringDir string, encryptFlag, noEncryptFlag bool, kekRef string, kmsConfig map[string]string) (*runner.EncryptionConfig, error) {
	if encryptFlag && noEncryptFlag {
		return nil, output.NewError("usage.conflicting_flags",
			"backup: --encrypt and --no-encrypt are mutually exclusive").
			Wrap(output.ErrUsage)
	}
	if noEncryptFlag {
		return nil, nil
	}

	// Cloud-KMS branch.  Triggered by an explicit --kek with
	// a non-local scheme.  We open the provider eagerly so an
	// auth/region misconfig surfaces here rather than mid-
	// backup; the runner closes it when TakeBackup returns.
	if kekRef != "" && kekRef != keystore.KEKRefLocal && keystore.SchemeOf(kekRef) != "local" {
		cfg := stringMapToAny(kmsConfig)
		provider, err := kms.DefaultRegistry.Open(ctx, kekRef, cfg)
		if err != nil {
			// An unreachable KMS endpoint exits 8 (kms.unreachable); a
			// credential/region/KEKRef misconfig keeps backup.kms_open_failed.
			return nil, kmsOpError(err,
				fmt.Sprintf("backup: open cloud KMS for %q", kekRef),
				"backup.kms_open_failed",
				&output.Suggestion{
					Human: "verify the KEKRef + the provider's --kms-config (region / endpoint / credentials)",
				})
		}
		return &runner.EncryptionConfig{
			Provider: provider,
			KEKRef:   provider.KEKRef(),
		}, nil
	}

	// Local-custody branch (the v0.1..shape).
	hasKEK := keystore.KEKExists(keyringDir)
	if encryptFlag && !hasKEK {
		return nil, output.NewError("backup.encrypt_no_kek",
			"backup: --encrypt set but no KEK file found at the keyring").
			WithSuggestion(&output.Suggestion{
				Human:   "generate a KEK by running init with --encrypt, or drop a 32-byte key at the keyring path manually",
				Command: "pg_hardstorage init --yes --encrypt",
			})
	}
	if !hasKEK {
		return nil, nil
	}
	kek, _, err := keystore.LoadOrGenerateKEK(keyringDir)
	if err != nil {
		return nil, output.NewError("backup.kek_load_failed",
			fmt.Sprintf("backup: load KEK: %v", err)).Wrap(err)
	}
	return &runner.EncryptionConfig{KEK: kek, KEKRef: keystore.KEKRefLocal}, nil
}

// stringMapToAny converts the cobra StringToStringVar output
// into a map[string]any the kms.Builder consumes.  Tries the
// obvious value coercions: "true"/"false" → bool, integer
// strings → int, everything else stays string.  Operators with
// complex configs use the YAML config file; --kms-config is
// the smallest CLI surface that covers the common cases.
func stringMapToAny(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true":
			out[k] = true
		case "false":
			out[k] = false
		default:
			out[k] = v
		}
	}
	return out
}

// backupResultBody is the typed body for `backup`'s success Result.
// Field order matches what we want users to read top-to-bottom in
// text mode (id first, then sizes, then storage location).
type backupResultBody struct {
	BackupID         string `json:"backup_id"`
	Deployment       string `json:"deployment"`
	Tenant           string `json:"tenant,omitempty"`
	PGVersion        int    `json:"pg_version"`
	SystemIdentifier string `json:"system_identifier"`
	StartLSN         string `json:"start_lsn"`
	StopLSN          string `json:"stop_lsn"`
	Timeline         uint32 `json:"timeline"`
	DurationMS       int64  `json:"duration_ms"`
	FileCount        int    `json:"file_count"`
	TablespaceCount  int    `json:"tablespace_count"`
	LogicalBytes     int64  `json:"logical_bytes"`
	UniqueChunkCount int    `json:"unique_chunk_count"`
	TotalChunkRefs   int    `json:"total_chunk_refs"`
	UniqueChunkBytes int64  `json:"unique_chunk_bytes"`
	PrimaryKey       string `json:"primary_key"`
	Encrypted        bool   `json:"encrypted"`
}

// WriteText is the text-renderer hook. Compact, scan-friendly output
// for the 3 a.m. operator who wants the headline numbers.
func (b backupResultBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ Backup committed\n")
	fmt.Fprintf(bw, "  ID:               %s\n", b.BackupID)
	fmt.Fprintf(bw, "  Deployment:       %s", b.Deployment)
	if b.Tenant != "" && b.Tenant != "default" {
		fmt.Fprintf(bw, " (tenant %s)", b.Tenant)
	}
	fmt.Fprintln(bw)
	fmt.Fprintf(bw, "  PostgreSQL:       %d\n", b.PGVersion)
	fmt.Fprintf(bw, "  Cluster ID:       %s\n", b.SystemIdentifier)
	fmt.Fprintf(bw, "  Stop LSN / TLI:   %s / %d\n", b.StopLSN, b.Timeline)
	fmt.Fprintf(bw, "  Files:            %d in %d tablespace(s)\n", b.FileCount, b.TablespaceCount)
	fmt.Fprintf(bw, "  Logical bytes:    %s\n", humanBytes(b.LogicalBytes))
	fmt.Fprintf(bw, "  Unique chunks:    %d (%s after dedup)\n", b.UniqueChunkCount, humanBytes(b.UniqueChunkBytes))
	if b.LogicalBytes > 0 && b.UniqueChunkBytes > 0 {
		ratio := float64(b.LogicalBytes) / float64(b.UniqueChunkBytes)
		fmt.Fprintf(bw, "  Dedup ratio:      %.2fx\n", ratio)
	}
	fmt.Fprintf(bw, "  Duration:         %d ms\n", b.DurationMS)
	if b.Encrypted {
		fmt.Fprintf(bw, "  Encryption:       AES-256-GCM (per-backup DEK, wrapped under local KEK)\n")
	} else {
		fmt.Fprintf(bw, "  Encryption:       none\n")
	}
	fmt.Fprintf(bw, "  Manifest:         %s", b.PrimaryKey)

	_, err := io.WriteString(w, bw.String())
	return err
}

// backupFileEventBody is the typed body for backup.file_archived
// events emitted under --verbose (issue #9).  Carries the same
// fields whether the renderer is JSON (structured map) or text
// (compact one-line via WriteText).
//
// "Compression %" was the issue's original wording but doesn't
// fit pg_hardstorage's CDC + dedup + chunk-envelope architecture
// — chunks dedup across files and zstd is applied at the
// envelope, not per-file.  We expose the closer-to-correct dedup
// metric instead: ChunkCount, DedupedChunks, and UniqueBytes (the
// bytes the CAS actually stored after dedup hits).
type backupFileEventBody struct {
	Path          string `json:"path"`
	Size          int64  `json:"size"`
	ChunkCount    int    `json:"chunk_count"`
	DedupedChunks int    `json:"deduped_chunks"`
	UniqueBytes   int64  `json:"unique_bytes"`
}

// WriteText renders the event body as one compact line so a
// `pg_hardstorage backup --verbose` run on a 50k-file PGDATA
// scrolls cleanly past on the operator's terminal:
//
//	base/16384/2619    8.0 KiB   1 chunk    (0 dedup,   8.0 KiB stored)
//
// The fixed-width left column for `path` is intentionally NOT
// padded — file path lengths vary wildly across PGDATA and any
// padding we picked would look broken on the next backup.
func (b backupFileEventBody) WriteText(w io.Writer) error {
	saved := b.Size - b.UniqueBytes
	if saved < 0 {
		saved = 0
	}
	_, err := fmt.Fprintf(w,
		"%s  size=%s  chunks=%d  deduped=%d  stored=%s",
		b.Path,
		humanBytes(b.Size),
		b.ChunkCount,
		b.DedupedChunks,
		humanBytes(b.UniqueBytes),
	)
	return err
}

// humanBytes formats n with the smallest binary-prefix unit that gives
// a sub-1024 mantissa. Up to TiB; anything larger renders as TiB.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit && exp < 3; x /= unit {
		div *= unit
		exp++
	}
	suffix := []string{"KiB", "MiB", "GiB", "TiB"}[exp]
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), suffix)
}

// backupCapacityPreflight runs the capacity gate before
// runner.Take. Three branches:
//
//  1. Fresh deployment (no prior manifest) → silent pass.
//     We have no projection; refusing here would block the
//     very first backup, which is the worst possible UX.
//
//  2. Backend doesn't expose FreeSpace (object stores) →
//     silent pass. The operator's quota is out-of-band.
//
//  3. Free space < projected × safety → refuse with
//     preflight.repo_full + a Suggestion pointing at
//     `repo gc` and the --ignore-capacity opt-out.
//
// Probe failures are fail-open: a flaky statfs shouldn't
// refuse an otherwise-OK backup. The structured event the
// dispatcher emits surfaces the failure for forensics.
func backupCapacityPreflight(ctx context.Context, opts runOptions, verifier *backup.Verifier) error {
	_, sp, err := openRepo(ctx, opts.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	projected, err := projectedBytesFromDeployment(ctx, sp, opts.deployment, verifier)
	if err != nil {
		// "no committed backups" is the fresh-deployment
		// case — silent pass. Other read failures (signature
		// errors, backend) are surfaced; we don't want to
		// silently skip the gate on a real problem.
		if isFreshDeploymentError(err) {
			return nil
		}
		return err
	}

	res, err := capacity.Preflight(ctx, sp, capacity.PreflightOptions{
		ProjectedBytes: projected,
		SafetyFactor:   opts.capacitySafetyFactor,
	})
	if err != nil {
		// Probe failed (statfs error). Fail-open per the
		// documented posture — the runtime emits an event
		// so monitoring sees the probe failure but the
		// backup proceeds.
		return nil
	}
	if res.Verdict == capacity.PreflightInsufficientSpace {
		return output.NewError("preflight.repo_full",
			fmt.Sprintf("backup: repo %s would not fit a %s backup (%s available, %s required with %.0f%% safety margin)",
				opts.repoURL,
				humanBytes(projected),
				humanBytes(res.AvailableBytes),
				humanBytes(res.RequiredBytes),
				(res.SafetyFactor-1.0)*100)).
			WithSuggestion(&output.Suggestion{
				Human:   "free repo space (run `repo gc --apply` to reclaim chunks unreferenced by retention) or pass --ignore-capacity if the projection is misleadingly large for this backup. The safety margin can be tuned with --capacity-safety-factor.",
				Command: "pg_hardstorage repo gc --apply --repo " + opts.repoURL,
			})
	}
	return nil
}

// isFreshDeploymentError reports whether err is the
// "deployment has no committed backups" sentinel from
// projectedBytesFromDeployment. The capacity pre-flight
// silently passes in that case (no projection = no gate).
func isFreshDeploymentError(err error) bool {
	if err == nil {
		return false
	}
	// Match the structured error code; cheap string check
	// keeps the dependency surface tight.
	return strings.Contains(err.Error(), "no committed backups")
}
