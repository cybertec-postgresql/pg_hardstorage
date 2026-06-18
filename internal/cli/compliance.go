// compliance.go — 'compliance report' CLI verb: time-windowed SOC2/ISO/HIPAA-friendly reports.
package cli

import (
	stdjson "encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/compliance"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// newComplianceCmd implements `pg_hardstorage compliance` — the
// time-windowed compliance-report surface.
//
// One subcommand today: `compliance report`. The compliance namespace
// is reserved for future verbs (e.g. `compliance retention-check`,
// `compliance evidence-bundle`) without disturbing the v1 contract.
func newComplianceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "compliance",
		Short: "Compliance reporting (SOC 2 / ISO 27001 / HIPAA / PCI / FedRAMP-friendly)",
		Long: `compliance produces forensics-grade reports about backup activity,
encryption coverage, KEK lifecycle, approval-workflow trail, hold
lifecycle, replica completeness, audit-chain integrity, and WORM
status — all in one read-only walk.

Today the only subcommand is ` + "`compliance report`" + `; the namespace is
reserved for additional reports without disturbing the v1 contract
on the existing one.`,
	}
	c.AddCommand(newComplianceReportCmd())
	return c
}

// newComplianceReportCmd implements `pg_hardstorage compliance report`.
func newComplianceReportCmd() *cobra.Command {
	var (
		repoURL    string
		deployment string
		since      string
		until      string
		format     string
		// Skip flags — defaults are "include the section". Quarterly
		// formal reports usually want everything; dashboard runs flip
		// off the expensive bits.
		skipBackups      bool
		skipEncryption   bool
		skipVerification bool
		skipKEKLifecycle bool
		skipApprovals    bool
		skipHolds        bool
		skipReplicas     bool
		skipChain        bool
		skipWORM         bool
		skipChainVerify  bool
	)
	c := &cobra.Command{
		Use:   "report <url>",
		Short: "Generate a time-windowed compliance report",
		Long: `Walks the repository over the named time window and produces a
structured report covering:

  - Backup activity (per-deployment + per-type counts; window-edge
    timestamps).
  - Encryption coverage (% of in-window backups encrypted; KEK ref
    breakdown; schemes used).
  - Verification coverage (verify.* audit events; per-deployment
    runs, oldest/last).
  - KEK lifecycle (kms.rotate / kms.shred events with actor / refs
    / tenant / timestamp).
  - Approval workflow (requests created; destructive ops executed
    in window).
  - Holds (hold.add / hold.remove / hold.expire counts).
  - Replica completeness (windowed primaries vs windowed replica
    copies; unreplicated count).
  - Audit chain (events in window; anchor count + last-anchor age;
    full chain-integrity verification result).
  - WORM mode + retention from HSREPO.

Window:
  --since   duration ("30d", "90d", "168h") OR RFC3339 timestamp.
            Default: now - 30 days.
  --until   same form. Default: now.

Format:
  --format json     (default) — JSON body, the v1 contract.
  --format markdown — forensics-grade Markdown, GFM tables, with
                      compliance-control hints next to each section
                      (e.g. SOC 2 CC6.7, ISO 27001 A.8.13). The text
                      renderer is the same when --output is text.

Sections are independently optional via --no-* flags so a fast
dashboard run skips the expensive walks. Read-only by construction;
safe at any cadence.`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if repoURL != "" && repoURL != args[0] {
					return output.NewError("usage.repo_conflict",
						"compliance report: --repo and the positional URL disagree").Wrap(output.ErrUsage)
				}
				repoURL = args[0]
			}
			return runComplianceReport(cmd, complianceReportFlags{
				repoURL:          repoURL,
				deployment:       deployment,
				since:            since,
				until:            until,
				format:           format,
				skipBackups:      skipBackups,
				skipEncryption:   skipEncryption,
				skipVerification: skipVerification,
				skipKEKLifecycle: skipKEKLifecycle,
				skipApprovals:    skipApprovals,
				skipHolds:        skipHolds,
				skipReplicas:     skipReplicas,
				skipChain:        skipChain,
				skipWORM:         skipWORM,
				skipChainVerify:  skipChainVerify,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (positional <url> also accepted)")
	c.Flags().StringVar(&deployment, "deployment", "",
		"restrict windowed sections to one deployment (repo-wide sections still cover everything)")
	c.Flags().StringVar(&since, "since", "",
		"window start: duration (24h, 30d) or RFC3339 timestamp; default = now - 30d")
	c.Flags().StringVar(&until, "until", "",
		"window end: duration or RFC3339 timestamp; default = now")
	c.Flags().StringVar(&format, "format", "json",
		"output format for the report body: json | markdown")
	c.Flags().BoolVar(&skipBackups, "no-backups", false, "skip the backup-activity section")
	c.Flags().BoolVar(&skipEncryption, "no-encryption", false, "skip the encryption-coverage section")
	c.Flags().BoolVar(&skipVerification, "no-verification", false, "skip the verification-coverage section")
	c.Flags().BoolVar(&skipKEKLifecycle, "no-kek-lifecycle", false, "skip the KEK-lifecycle section")
	c.Flags().BoolVar(&skipApprovals, "no-approvals", false, "skip the approval-workflow section")
	c.Flags().BoolVar(&skipHolds, "no-holds", false, "skip the holds section")
	c.Flags().BoolVar(&skipReplicas, "no-replicas", false, "skip the replica-completeness section")
	c.Flags().BoolVar(&skipChain, "no-chain", false, "skip the audit-chain section")
	c.Flags().BoolVar(&skipWORM, "no-worm", false, "skip the WORM section")
	c.Flags().BoolVar(&skipChainVerify, "no-chain-verify", false,
		"keep the chain section's event/anchor counts but skip the (potentially expensive) full VerifyChain pass")
	return c
}

type complianceReportFlags struct {
	repoURL          string
	deployment       string
	since            string
	until            string
	format           string
	skipBackups      bool
	skipEncryption   bool
	skipVerification bool
	skipKEKLifecycle bool
	skipApprovals    bool
	skipHolds        bool
	skipReplicas     bool
	skipChain        bool
	skipWORM         bool
	skipChainVerify  bool
}

func runComplianceReport(cmd *cobra.Command, f complianceReportFlags) error {
	d := DispatcherFrom(cmd)
	// Positional-or-flag: guard the resolved value, not the flag.
	if f.repoURL == "" {
		return missingFlagErr(cmd, "--repo (or the URL positionally)")
	}
	since, err := parseSinceUntil(f.since)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("compliance report: --since: %v", err)).Wrap(output.ErrUsage)
	}
	until, err := parseSinceUntil(f.until)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("compliance report: --until: %v", err)).Wrap(output.ErrUsage)
	}
	switch f.format {
	case "", "json", "markdown":
	default:
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("compliance report: --format must be json or markdown; got %q", f.format)).
			Wrap(output.ErrUsage)
	}

	verifier, err := loadVerifier()
	if err != nil {
		return err
	}
	meta, sp, err := openRepo(cmd.Context(), f.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	rep, err := compliance.Generate(cmd.Context(), sp, meta, f.repoURL, compliance.Options{
		Verifier:         verifier,
		Since:            since,
		Until:            until,
		DeploymentFilter: f.deployment,
		SkipBackups:      f.skipBackups,
		SkipEncryption:   f.skipEncryption,
		SkipVerification: f.skipVerification,
		SkipKEKLifecycle: f.skipKEKLifecycle,
		SkipApprovals:    f.skipApprovals,
		SkipHolds:        f.skipHolds,
		SkipReplicas:     f.skipReplicas,
		SkipChain:        f.skipChain,
		SkipWORM:         f.skipWORM,
		SkipChainVerify:  f.skipChainVerify,
	})
	if err != nil {
		return output.NewError("compliance.report_failed",
			fmt.Sprintf("compliance report: %v", err)).Wrap(err)
	}

	body := complianceReportBody{
		Report: rep,
		format: f.format,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// complianceReportBody wraps the domain Report with renderer hooks
// for both JSON (the v1 contract) and Markdown (forensics output).
//
// The format flag picks which text renderer applies:
//   - format=json + -o text   → readable summary (compact text)
//   - format=markdown + -o text → Markdown body
//   - -o json (any format)     → JSON body (v1 contract)
//
// JSON output is always the underlying domain Report (via
// MarshalJSON), regardless of format. JSON consumers parse a single
// stable shape and never see the wrapper.
type complianceReportBody struct {
	*compliance.Report
	format string
}

// MarshalJSON forwards to the domain Report so the JSON shape is the
// v1 contract — operators parsing the body don't see a wrapper.
func (b complianceReportBody) MarshalJSON() ([]byte, error) {
	return stdjson.Marshal(b.Report)
}

// WriteText renders the report. Picks Markdown vs compact text per
// the format flag.
func (b complianceReportBody) WriteText(w io.Writer) error {
	if strings.EqualFold(b.format, "markdown") {
		return compliance.RenderMarkdown(w, b.Report)
	}
	return writeComplianceReportSummary(w, b.Report)
}

// writeComplianceReportSummary renders a compact human-readable
// summary for `-o text --format json` (the default + text combo).
// Markdown is reserved for `--format markdown`; this is the
// "single-screen overview" view.
func writeComplianceReportSummary(w io.Writer, r *compliance.Report) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "compliance report — %s\n", r.URL)
	fmt.Fprintf(bw, "  Window: %s → %s\n",
		r.Since.Format(time.RFC3339), r.Until.Format(time.RFC3339))
	if r.DeploymentFilter != "" {
		fmt.Fprintf(bw, "  Filter: deployment %q\n", r.DeploymentFilter)
	}
	if r.Repo != nil && r.Repo.WORMMode != "" {
		fmt.Fprintf(bw, "  WORM:   %s (%s)\n", r.Repo.WORMMode,
			(time.Duration(r.Repo.WORMRetentionSeconds) * time.Second).String())
	}
	fmt.Fprintf(bw, "  Walk:   %d ms\n", r.DurationMS)
	fmt.Fprintln(bw)
	if r.Backups != nil {
		fmt.Fprintf(bw, "Backups committed:    %d\n", r.Backups.TotalCommitted)
	}
	if r.Encryption != nil {
		fmt.Fprintf(bw, "Encryption coverage:  %s (%d/%d)\n",
			compliance.FormatPercent(r.Encryption.CoveragePercent),
			r.Encryption.EncryptedCount,
			r.Encryption.EncryptedCount+r.Encryption.UnencryptedCount)
	}
	if r.Verification != nil {
		fmt.Fprintf(bw, "Verifications run:    %d\n", r.Verification.TotalRuns)
	}
	if r.KEKLifecycle != nil {
		fmt.Fprintf(bw, "KEK rotations:        %d (shreds %d)\n",
			r.KEKLifecycle.RotationsAttempted, r.KEKLifecycle.ShredsAttempted)
	}
	if r.Approvals != nil {
		fmt.Fprintf(bw, "Destructive ops:      %d (approval requests %d)\n",
			r.Approvals.DestructiveOps, r.Approvals.RequestsCreated)
	}
	if r.Holds != nil {
		fmt.Fprintf(bw, "Holds add/remove/exp: %d / %d / %d\n",
			r.Holds.HoldsAdded, r.Holds.HoldsRemoved, r.Holds.HoldsExpired)
	}
	if r.Replicas != nil {
		if r.Replicas.WindowedPrimaries > 0 {
			pct := float64(r.Replicas.WindowedReplicaCopies) * 100.0 /
				float64(r.Replicas.WindowedPrimaries)
			fmt.Fprintf(bw, "Replicas covered:     %s (%d unreplicated)\n",
				compliance.FormatPercent(pct), r.Replicas.UnreplicatedInWindow)
		}
	}
	if r.Chain != nil {
		verdict := "PASS"
		if !r.Chain.VerifyOK && r.Chain.VerifyEventsChecked > 0 {
			verdict = fmt.Sprintf("FAIL (%d hashes, %d breaks)",
				r.Chain.VerifyHashMismatches, r.Chain.VerifyChainBreaks)
		}
		fmt.Fprintf(bw, "Chain verify:         %s (%d events checked)\n",
			verdict, r.Chain.VerifyEventsChecked)
	}
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "Use --format markdown for the full forensics-grade report.")
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
