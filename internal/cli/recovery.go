// recovery.go — CLI surface for recovery readiness reports.
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

// newRecoveryCmd implements `pg_hardstorage recovery` — the
// recovery-toolkit subcommand suite. Two verbs today:
//
//   - readiness <deployment>     — recovery-readiness scorecard
//   - windows <deployment>        — PITR windows enumeration
//
// The natural entry point for the 3am operator wondering "if I had
// to recover this deployment right now, would it work?".
func newRecoveryCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "recovery",
		Short: "Recovery-toolkit: readiness scorecard, PITR windows, and more",
		Long: `Read-only diagnostics for the recovery side of the system.

  recovery readiness <deployment>   — single-screen scorecard
                                       answering "could we restore
                                       this deployment right now?"
  recovery windows <deployment>     — every available PITR window,
                                       with WAL-coverage gaps
                                       called out

Different from doctor (host / config / connectivity), verify (one
backup's bytes), and restore --preview (one specific restore plan):
recovery answers "can we recover", "how long would it take", and
"what windows are available?".

Read-only by construction; safe at any cadence.`,
	}
	c.AddCommand(newRecoveryReadinessCmd())
	c.AddCommand(newRecoveryWindowsCmd())
	c.AddCommand(newRecoveryDrillCmd())
	return c
}

// newRecoveryReadinessCmd implements `recovery readiness`.
func newRecoveryReadinessCmd() *cobra.Command {
	var (
		repoURL           string
		format            string
		assumedThroughput string
		stalenessWindow   string
		rpoTargetSeconds  int64
		rtoTargetSeconds  int64
		skipVerification  bool
		skipEncryption    bool
		skipWAL           bool
	)
	c := &cobra.Command{
		Use:   "readiness <deployment>",
		Short: "Single-screen recovery-readiness scorecard for one deployment",
		Long: `recovery readiness aggregates many signals (latest backup age,
RTO estimate, verification freshness, KEK reachability, WAL
coverage) into one structured report with a traffic-light verdict
(ready / ready_with_warnings / not_ready / no_backups).

Tunable thresholds:
  --rpo-seconds N     RPO target; observed-vs-target comparison.
                      Default: read from the deployment's SLO
                      config when present.
  --rto-seconds N     RTO target.  Default: same.
  --assumed-throughput  Bytes/sec used for the RTO estimate
                      (e.g. "160MiB", "1GiB", "100MB"); default
                      160 MiB/s.
  --staleness 7d      Verification record older than this is "stale".

Skip flags suppress optional sections:
  --no-verification    skip the verification.json freshness check
  --no-encryption      skip the KEK-reachability check (e.g. when
                       the operator runs offline without the keyring)
  --no-wal             skip the WAL-coverage walk

Output formats:
  --format json     (default) — JSON body, the v1 contract.
  --format markdown — forensics-grade GFM Markdown rendering.

Read-only; safe at any cadence.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRecoveryReadiness(cmd, args[0], recoveryReadinessFlags{
				repoURL:           repoURL,
				format:            format,
				assumedThroughput: assumedThroughput,
				stalenessWindow:   stalenessWindow,
				rpoTargetSeconds:  rpoTargetSeconds,
				rtoTargetSeconds:  rtoTargetSeconds,
				skipVerification:  skipVerification,
				skipEncryption:    skipEncryption,
				skipWAL:           skipWAL,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&format, "format", "json",
		"output format for the report body: json | markdown")
	c.Flags().StringVar(&assumedThroughput, "assumed-throughput", "",
		"bytes/sec for the RTO estimate (e.g. 160MiB, 1GiB); default 160 MiB/s")
	c.Flags().StringVar(&stalenessWindow, "staleness", "",
		"verification staleness window (e.g. 7d, 24h); default 7d")
	c.Flags().Int64Var(&rpoTargetSeconds, "rpo-seconds", 0,
		"RPO target in seconds (0 = no target check)")
	c.Flags().Int64Var(&rtoTargetSeconds, "rto-seconds", 0,
		"RTO target in seconds (0 = no target check)")
	c.Flags().BoolVar(&skipVerification, "no-verification", false, "skip the verification freshness check")
	c.Flags().BoolVar(&skipEncryption, "no-encryption", false, "skip the KEK reachability check")
	c.Flags().BoolVar(&skipWAL, "no-wal", false, "skip the WAL coverage check")
	return c
}

type recoveryReadinessFlags struct {
	repoURL           string
	format            string
	assumedThroughput string
	stalenessWindow   string
	rpoTargetSeconds  int64
	rtoTargetSeconds  int64
	skipVerification  bool
	skipEncryption    bool
	skipWAL           bool
}

func runRecoveryReadiness(cmd *cobra.Command, deployment string, f recoveryReadinessFlags) error {
	d := DispatcherFrom(cmd)
	switch f.format {
	case "", "json", "markdown":
	default:
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("recovery readiness: --format must be json or markdown; got %q", f.format)).
			Wrap(output.ErrUsage)
	}

	throughput, err := parseThroughput(f.assumedThroughput)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("recovery readiness: --assumed-throughput: %v", err)).Wrap(output.ErrUsage)
	}
	staleness, err := parseRecoveryDuration(f.stalenessWindow)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("recovery readiness: --staleness: %v", err)).Wrap(output.ErrUsage)
	}
	if f.rpoTargetSeconds < 0 || f.rtoTargetSeconds < 0 {
		return output.NewError("usage.bad_flag",
			"recovery readiness: --rpo-seconds / --rto-seconds must be >= 0").Wrap(output.ErrUsage)
	}

	verifier, err := loadVerifier()
	if err != nil {
		return err
	}
	_, sp, err := openRepo(cmd.Context(), f.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return output.NewError("internal", err.Error()).Wrap(err)
	}

	opts := recovery.Options{
		Verifier:                    verifier,
		AssumedThroughput:           throughput,
		VerificationStalenessWindow: staleness,
		RPOTargetSeconds:            f.rpoTargetSeconds,
		RTOTargetSeconds:            f.rtoTargetSeconds,
		SkipVerification:            f.skipVerification,
		SkipEncryption:              f.skipEncryption,
		SkipWAL:                     f.skipWAL,
	}
	if !f.skipEncryption {
		opts.KEKResolver = recovery.KeystoreKEKResolver(p.Keyring.Value)
	}

	r, err := recovery.Readiness(cmd.Context(), sp, deployment, opts)
	if err != nil {
		return output.NewError("recovery.readiness_failed",
			fmt.Sprintf("recovery readiness: %v", err)).Wrap(err)
	}
	r.URL = f.repoURL

	body := readinessReportBody{
		ReadinessReport: r,
		format:          f.format,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// parseThroughput accepts a human byte-rate string. Empty → 0
// (use default).
func parseThroughput(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	// Accept "160MiB" / "1GiB" / "100MB" / "1024".
	mult := int64(1)
	num := s
	switch {
	case strings.HasSuffix(s, "GiB"):
		mult = 1 << 30
		num = strings.TrimSuffix(s, "GiB")
	case strings.HasSuffix(s, "MiB"):
		mult = 1 << 20
		num = strings.TrimSuffix(s, "MiB")
	case strings.HasSuffix(s, "KiB"):
		mult = 1 << 10
		num = strings.TrimSuffix(s, "KiB")
	case strings.HasSuffix(s, "GB"):
		mult = 1_000_000_000
		num = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		mult = 1_000_000
		num = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KB"):
		mult = 1_000
		num = strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "B"):
		num = strings.TrimSuffix(s, "B")
	}
	num = strings.TrimSpace(num)
	var n int64
	if _, err := fmt.Sscanf(num, "%d", &n); err != nil {
		return 0, fmt.Errorf("expected number with unit (160MiB, 1GiB); got %q", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("negative throughput")
	}
	return n * mult, nil
}

// parseRecoveryDuration accepts time.ParseDuration values + Nd
// (days) form. Same parser shape as forecast's.
func parseRecoveryDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	if strings.HasSuffix(s, "d") {
		var n int
		if _, err := fmt.Sscanf(s, "%dd", &n); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	return 0, fmt.Errorf("expected duration (e.g. 7d, 24h); got %q", s)
}

// readinessReportBody wraps recovery.ReadinessReport for the dual
// JSON / Markdown output flow.
type readinessReportBody struct {
	*recovery.ReadinessReport
	format string
}

// MarshalJSON emits the embedded recovery.ReadinessReport so the JSON contract
// stays the domain v1 shape.
func (b readinessReportBody) MarshalJSON() ([]byte, error) {
	return stdjson.Marshal(b.ReadinessReport)
}

// WriteText renders the readiness report to w, choosing the markdown variant
// when format is "markdown" and the compact summary otherwise.
func (b readinessReportBody) WriteText(w io.Writer) error {
	if strings.EqualFold(b.format, "markdown") {
		return recovery.RenderReadinessMarkdown(w, b.ReadinessReport)
	}
	return writeReadinessSummary(w, b.ReadinessReport)
}

// writeReadinessSummary renders the compact human view for
// `--format json -o text`.
func writeReadinessSummary(w io.Writer, r *recovery.ReadinessReport) error {
	bw := &strings.Builder{}
	icon := "✓"
	switch r.OverallStatus {
	case recovery.StatusReadyWithWarn:
		icon = "·"
	case recovery.StatusNotReady, recovery.StatusNoBackups:
		icon = "✗"
	}
	fmt.Fprintf(bw, "recovery readiness — %s/%s\n", r.URL, r.Deployment)
	fmt.Fprintf(bw, "  %s Status: %s (%d issue(s))\n", icon, strings.ToUpper(string(r.OverallStatus)), len(r.Issues))
	fmt.Fprintf(bw, "  Backups: %d\n", r.BackupCount)
	if r.Latest != nil {
		fmt.Fprintf(bw, "  Latest: %s (%s ago, %s)\n",
			r.Latest.BackupID,
			(time.Duration(r.Latest.AgeSeconds) * time.Second).Truncate(time.Second),
			humanBytes(r.Latest.LogicalBytes))
	}
	if r.RPO != nil {
		descr := ""
		if r.RPO.TargetSeconds > 0 {
			ic := "✓"
			if !r.RPO.Met {
				ic = "✗"
			}
			descr = fmt.Sprintf(" (%s target %ds)", ic, r.RPO.TargetSeconds)
		}
		fmt.Fprintf(bw, "  RPO observed: %ds%s\n", r.RPO.ObservedSeconds, descr)
	}
	if r.RTO != nil {
		descr := ""
		if r.RTO.TargetSeconds > 0 {
			ic := "✓"
			if !r.RTO.Met {
				ic = "✗"
			}
			descr = fmt.Sprintf(" (%s target %ds)", ic, r.RTO.TargetSeconds)
		}
		fmt.Fprintf(bw, "  RTO estimate: %ds%s at %s/s\n",
			r.RTO.EstimatedSeconds, descr,
			humanBytes(r.RTO.AssumedThroughputBytes))
	}
	if r.Encryption != nil && r.Encryption.Encrypted {
		ic := "✓"
		if !r.Encryption.KEKReachable {
			ic = "✗"
		}
		fmt.Fprintf(bw, "  KEK %s reachable: %s\n", r.Encryption.KEKRef, ic)
	}
	if r.WAL != nil && r.WAL.HasGapPersisted {
		fmt.Fprintf(bw, "  ✗ Persisted WAL gap: %d bytes (%s..%s)\n",
			r.WAL.GapBytes, r.WAL.GapStartLSN, r.WAL.GapEndLSN)
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
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "Use --format markdown for the full report.")
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
