// recovery_drill_cmd.go — CLI surface for executing a recovery drill.
package cli

import (
	stdjson "encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/recovery"
)

// newRecoveryDrillCmd implements `recovery drill <deployment>`.
//
// The execution side of the recovery toolkit.  `recovery
// readiness` answers "could we recover?"; `recovery windows`
// answers "what windows are available?"; `recovery drill`
// actually runs a full restore-and-verify cycle and reports the
// RTO actual + verify outcome.
//
// Five phases:
//  1. pick     — resolve "latest" or explicit BackupID to a manifest
//  2. prepare  — mkdtemp the target dir
//  3. restore  — call internal/restore.Restore against the temp dir
//  4. verify   — internal/verify/sandbox runs pg_verifybackup
//  5. teardown — os.RemoveAll the temp dir (skipped with --keep)
//
// Verdicts: pass / partial / fail.  Fail when any phase errors;
// partial when verify is skipped (with --allow-skip-verify or
// --skip-verify).  RTO actual is the wallclock from drill start
// to successful restore.
func newRecoveryDrillCmd() *cobra.Command {
	var (
		repoURL            string
		backupID           string
		pgMajor            string
		sandboxImage       string
		tempBaseDir        string
		keepTargetDir      bool
		allowSkipVerify    bool
		skipVerifyEntirely bool
		rtoSeconds         int64
		format             string
		skipEncryption     bool
		skipHistory        bool
		operator           string
	)
	c := &cobra.Command{
		Use:   "drill <deployment>",
		Short: "Run a full restore-and-verify drill against a real backup",
		Long: `Pick a backup, restore into a temporary target dir, run
pg_verifybackup against the restored data dir, and tear down.
Reports RTO actual + verify outcome.

The drill exercises the full critical path that a real recovery
would take.  Read-only against the source repo; writes only to a
temporary target dir which is removed on completion (unless
--keep is set).

Backup selection:
  <empty> | latest    pick the freshest committed backup
  <backup-id>          pick the named manifest (must be visible)

Verify control:
  --pg-major NN         override the sandbox image (default: derive from manifest)
  --image IMG           override 'postgres:<major>' (air-gapped registries)
  --allow-skip-verify   accept a Partial verdict when pg_verifybackup
                        skips (e.g. older manifest with no on-disk
                        backup_manifest)
  --skip-verify         strongest opt-out — don't even try to spin up
                        the sandbox.  Returns Partial verdict with
                        the restore-only result.

Output formats:
  --format json     (default) — JSON body, the v1 contract.
  --format markdown — forensics-grade GFM rendering.

The drill requires Docker on the operator host (for the sandbox).
For Docker-free runs, pass --skip-verify; the report's RTO actual
+ restore-phase detail are still useful for capacity planning.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRecoveryDrill(cmd, args[0], recoveryDrillFlags{
				repoURL:            repoURL,
				backupID:           backupID,
				pgMajor:            pgMajor,
				sandboxImage:       sandboxImage,
				tempBaseDir:        tempBaseDir,
				keepTargetDir:      keepTargetDir,
				allowSkipVerify:    allowSkipVerify,
				skipVerifyEntirely: skipVerifyEntirely,
				rtoSeconds:         rtoSeconds,
				format:             format,
				skipEncryption:     skipEncryption,
				skipHistory:        skipHistory,
				operator:           operator,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&backupID, "backup-id", "",
		"explicit backup ID to drill against (default: latest)")
	c.Flags().StringVar(&pgMajor, "pg-major", "",
		"override sandbox PG major version (default: derive from manifest)")
	c.Flags().StringVar(&sandboxImage, "image", "",
		"override 'postgres:<major>' sandbox image (air-gapped)")
	c.Flags().StringVar(&tempBaseDir, "temp-base", "",
		"parent directory for the temporary target dir (default: $TMPDIR)")
	c.Flags().BoolVar(&keepTargetDir, "keep", false,
		"keep the temporary target dir after the drill (for inspection)")
	c.Flags().BoolVar(&allowSkipVerify, "allow-skip-verify", false,
		"accept a Partial verdict when pg_verifybackup skips")
	c.Flags().BoolVar(&skipVerifyEntirely, "skip-verify", false,
		"don't run the sandbox verify at all (Docker-free run)")
	c.Flags().Int64Var(&rtoSeconds, "rto-seconds", 0,
		"RTO target in seconds for actual-vs-target comparison")
	c.Flags().StringVar(&format, "format", "json",
		"output format: json | markdown")
	c.Flags().BoolVar(&skipEncryption, "no-encryption", false,
		"don't wire the local-keystore KEK resolver (drill an unencrypted backup or one whose KEK lives elsewhere)")
	c.Flags().BoolVar(&skipHistory, "no-history", false,
		"suppress the auto-persist of a slim drill history entry into recovery/drills/")
	c.Flags().StringVar(&operator, "operator", "",
		"record the operator identity in the history entry (free-form; cron jobs typically pass scheduler:<task-id>)")
	// `drill history` is a subcommand for the auto-persisted
	// drill-history surface.  Operators run `recovery drill history
	// <deployment>` to see trend / RTO distribution / latest verdict
	// across past drills.
	c.AddCommand(newRecoveryDrillHistoryCmd())
	return c
}

type recoveryDrillFlags struct {
	repoURL            string
	backupID           string
	pgMajor            string
	sandboxImage       string
	tempBaseDir        string
	keepTargetDir      bool
	allowSkipVerify    bool
	skipVerifyEntirely bool
	rtoSeconds         int64
	format             string
	skipEncryption     bool
	skipHistory        bool
	operator           string
}

func runRecoveryDrill(cmd *cobra.Command, deployment string, f recoveryDrillFlags) error {
	d := DispatcherFrom(cmd)
	switch f.format {
	case "", "json", "markdown":
	default:
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("recovery drill: --format must be json or markdown; got %q", f.format)).
			Wrap(output.ErrUsage)
	}
	if f.rtoSeconds < 0 {
		return output.NewError("usage.bad_flag",
			"recovery drill: --rto-seconds must be >= 0").Wrap(output.ErrUsage)
	}
	if f.skipVerifyEntirely && f.allowSkipVerify {
		return output.NewError("usage.bad_flag",
			"recovery drill: --skip-verify and --allow-skip-verify are mutually exclusive (--skip-verify is the strongest opt-out)").
			Wrap(output.ErrUsage)
	}

	verifier, err := loadVerifier()
	if err != nil {
		return err
	}
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return output.NewError("internal", err.Error()).Wrap(err)
	}

	opts := recovery.DrillOptions{
		Verifier:           verifier,
		BackupID:           f.backupID,
		PGMajor:            f.pgMajor,
		SandboxImage:       f.sandboxImage,
		TempBaseDir:        f.tempBaseDir,
		KeepTargetDir:      f.keepTargetDir,
		AllowSkipVerify:    f.allowSkipVerify,
		SkipVerifyEntirely: f.skipVerifyEntirely,
		RTOEstimateSeconds: f.rtoSeconds,
		SkipHistory:        f.skipHistory,
		Operator:           f.operator,
	}
	if !f.skipEncryption {
		opts.KEKResolver = recovery.KeystoreKEKResolver(p.Keyring.Value)
		opts.DEKUnwrapper = recovery.KeystoreDEKResolver(p.Keyring.Value)
	}

	r, err := recovery.Drill(cmd.Context(), f.repoURL, deployment, opts)
	if err != nil {
		return output.NewError("recovery.drill_failed",
			fmt.Sprintf("recovery drill: %v", err)).Wrap(err)
	}

	body := drillReportBody{DrillReport: r, format: f.format}
	if rerr := d.Result(output.NewResult(cmd.CommandPath()).WithBody(body)); rerr != nil {
		return rerr
	}

	// A drill verdict of fail trips ExitVerifyFailed (9) — same
	// posture as `verify`.  partial + pass exit 0.  This lets a
	// cron / scheduler-driven `drill` job alarm without parsing
	// the body.
	if r.Verdict == recovery.DrillVerdictFail {
		human := "review the failures slice in the JSON body — see the Phases table for which phase failed; the Issues section names the structured codes + suggestions."
		return output.NewError("verify.drill_failed",
			fmt.Sprintf("recovery drill: %s/%s — verdict: %s (%d issue(s))",
				deployment, r.BackupID, r.Verdict, len(r.Issues))).
			WithSuggestion(&output.Suggestion{Human: human})
	}
	return nil
}

// drillReportBody wraps recovery.DrillReport for the dual JSON /
// Markdown output flow.  JSON always emits the underlying Report
// verbatim via MarshalJSON so consumers see only the v1 contract.
type drillReportBody struct {
	*recovery.DrillReport
	format string
}

// MarshalJSON emits the embedded recovery.DrillReport so the JSON contract
// stays the domain v1 shape.
func (b drillReportBody) MarshalJSON() ([]byte, error) {
	return stdjson.Marshal(b.DrillReport)
}

// WriteText renders the drill report to w, choosing the markdown variant when
// format is "markdown" and the compact summary otherwise.
func (b drillReportBody) WriteText(w io.Writer) error {
	if strings.EqualFold(b.format, "markdown") {
		return recovery.RenderDrillMarkdown(w, b.DrillReport)
	}
	return writeDrillSummary(w, b.DrillReport)
}

// writeDrillSummary renders the compact human-readable view for
// `--format json -o text`.
func writeDrillSummary(w io.Writer, r *recovery.DrillReport) error {
	bw := &strings.Builder{}
	icon := "✓"
	switch r.Verdict {
	case recovery.DrillVerdictPartial:
		icon = "·"
	case recovery.DrillVerdictFail:
		icon = "✗"
	}
	fmt.Fprintf(bw, "recovery drill — %s/%s\n", r.URL, r.Deployment)
	fmt.Fprintf(bw, "  %s Verdict: %s (%d issue(s))\n",
		icon, strings.ToUpper(string(r.Verdict)), len(r.Issues))
	if r.BackupID != "" {
		fmt.Fprintf(bw, "  Backup:  %s\n", r.BackupID)
	}
	if r.RTOActualSeconds > 0 {
		fmt.Fprintf(bw, "  RTO actual:    %ds\n", r.RTOActualSeconds)
	}
	if r.RTOEstimateSeconds > 0 {
		fmt.Fprintf(bw, "  RTO estimate:  %ds\n", r.RTOEstimateSeconds)
	}
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "Phases:")
	for _, p := range r.Phases {
		ic := "✓"
		if !p.OK {
			ic = "✗"
		}
		dur := time.Duration(p.DurationMS) * time.Millisecond
		descr := p.Note
		if p.Error != "" {
			descr = "ERROR: " + p.Error
		}
		fmt.Fprintf(bw, "  %s %-9s %-10s %s\n", ic, p.Name, dur.Truncate(time.Millisecond), descr)
	}
	if r.Restore != nil {
		fmt.Fprintln(bw)
		fmt.Fprintf(bw, "Restore: %d files, %s, %d chunks\n",
			r.Restore.FileCount, humanBytes(r.Restore.BytesWritten), r.Restore.ChunksFetched)
	}
	if r.Verify != nil {
		passDescr := "passed"
		if r.Verify.Skipped {
			passDescr = "skipped (" + r.Verify.SkipReason + ")"
		} else if !r.Verify.Passed {
			passDescr = "FAILED"
		}
		fmt.Fprintf(bw, "Verify: %s (%s)\n", passDescr, r.Verify.Image)
	}
	if len(r.Issues) > 0 {
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "Issues:")
		issues := append([]recovery.ReadinessIssue(nil), r.Issues...)
		recovery.SortIssues(issues)
		for _, i := range issues {
			fmt.Fprintf(bw, "  [%s] %s — %s\n", i.Severity, i.Code, i.Message)
			if i.Suggestion != "" {
				fmt.Fprintf(bw, "      → %s\n", i.Suggestion)
			}
		}
	}
	if r.TargetDir != "" {
		fmt.Fprintln(bw)
		fmt.Fprintf(bw, "Target dir kept: %s\n", r.TargetDir)
	}
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "Use --format markdown for the full report.")
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
