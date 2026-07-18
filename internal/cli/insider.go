// insider.go — CLI surface for insider-risk anomaly scans over the audit log.
package cli

import (
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/insider"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// newInsiderCmd builds `pg_hardstorage insider`: insider-threat
// anomaly detection on top of the hash-chained audit log.
//
//	insider scan --repo R [--baseline DUR] [--target DUR]   — run + persist
//	insider list --repo R                                    — newest-first
//	insider show <id>                                        — full body
func newInsiderCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "insider",
		Short: "Insider-threat detection: scan the audit log for unusual patterns",
		Long: `Detect insider-threat patterns in the audit log:

  - novel_principal           an actor not seen in baseline appears in target
  - first_destructive_action  an actor performs a destructive action they
                              never performed in baseline (CRITICAL)
  - off_hours_destructive     destructive action at an UTC hour the actor
                              has never used in baseline
  - volume_spike              actor's per-action rate exceeds baseline by
                              the configured factor
  - cross_tenant_novel        actor touches a tenant they haven't before
  - post_jit_destructive      destructive action within 1 h of a jit.issue
                              (break-glass pattern, NOTICE-level for record)

Each scan compares a baseline window (default 30 d) against a target
window (default 24 h).  Findings carry severity, actor, action, the
audit event IDs that triggered them, and a human-readable reason.`,
	}
	c.AddCommand(
		newInsiderScanCmd(),
		newInsiderListCmd(),
		newInsiderShowCmd(),
	)
	return c
}

// ----- scan -----

func newInsiderScanCmd() *cobra.Command {
	var (
		repoURL  string
		baseline time.Duration
		target   time.Duration
		tenant   string
		note     string
		factor   float64
		failOn   string
	)
	c := &cobra.Command{
		Use:   "scan",
		Short: "Run one insider-threat scan; persist the result",
		Long: `Run the detection rules against the audit log.  The scan body
is persisted at insider/scans/<id>.json so a future commit can
reference it from a threshold attestation or a compliance report.

Exit codes:
  0 — no findings (or every finding is below --fail-on severity)
  9 — at least one finding meets/exceeds --fail-on severity (default warning)`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInsiderScan(cmd, insiderScanFlags{
				repoURL: repoURL, baseline: baseline, target: target,
				tenant: tenant, note: note, factor: factor,
				failOn: failOn,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().DurationVar(&baseline, "baseline", insider.DefaultBaselineDuration,
		"baseline window length (e.g. 720h for 30d)")
	c.Flags().DurationVar(&target, "target", insider.DefaultTargetDuration,
		"target window length (e.g. 24h)")
	c.Flags().StringVar(&tenant, "tenant", "",
		"restrict scan to one tenant (default: every tenant)")
	c.Flags().StringVar(&note, "note", "",
		"operator note recorded with the scan (e.g. 'daily cron')")
	c.Flags().Float64Var(&factor, "spike-factor", insider.DefaultVolumeSpikeFactor,
		"target rate must exceed baseline rate by this factor to flag a spike")
	c.Flags().StringVar(&failOn, "fail-on", "warning",
		"minimum severity that flips the exit code to 9: info | notice | warning | critical | none")
	return c
}

type insiderScanFlags struct {
	repoURL  string
	baseline time.Duration
	target   time.Duration
	tenant   string
	note     string
	factor   float64
	failOn   string
}

func runInsiderScan(cmd *cobra.Command, f insiderScanFlags) error {
	d := DispatcherFrom(cmd)
	if !finiteFloat(f.factor) || f.factor <= 0 {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("insider scan: --spike-factor must be a finite value > 0; got %v", f.factor)).Wrap(output.ErrUsage)
	}
	failOn, err := parseFailOnSeverity(f.failOn)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("insider scan: --fail-on: %v", err)).Wrap(output.ErrUsage)
	}
	repoMeta, sp, err := openRepo(cmd.Context(), f.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	if err := assertRepoWritable(cmd.Context(), sp, "insider scan"); err != nil {
		return err
	}
	auditStore := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	det := insider.NewDetector(auditStore)
	scan, err := det.Run(cmd.Context(), insider.Options{
		BaselineWindow:    f.baseline,
		TargetWindow:      f.target,
		Tenant:            f.tenant,
		Note:              f.note,
		VolumeSpikeFactor: f.factor,
	})
	if err != nil {
		if errors.Is(err, insider.ErrInvalidWindow) {
			return output.NewError("usage.bad_flag",
				fmt.Sprintf("insider scan: %v", err)).Wrap(output.ErrUsage)
		}
		return output.NewError("insider.scan_failed",
			fmt.Sprintf("insider scan: %v", err)).Wrap(err)
	}
	if err := insider.NewScanStore(sp).Put(cmd.Context(), scan); err != nil {
		return output.NewError("insider.put_failed",
			fmt.Sprintf("insider scan: persist: %v", err)).Wrap(err)
	}
	// Best-effort audit append.
	_ = auditStore.Append(cmd.Context(), &audit.Event{
		Action:    "insider.scan",
		Timestamp: time.Now().UTC(),
		Body: map[string]any{
			"scan_id":          scan.ID,
			"tenant":           f.tenant,
			"baseline_events":  scan.BaselineEvents,
			"target_events":    scan.TargetEvents,
			"finding_count":    len(scan.Findings),
			"highest_severity": string(scan.HighestSeverity()),
		},
	})
	body := insiderScanBody{Scan: scan}
	if rerr := d.Result(output.NewResult(cmd.CommandPath()).WithBody(body)); rerr != nil {
		return rerr
	}
	if shouldFail(scan.HighestSeverity(), failOn) {
		return output.NewError("verify.insider_findings",
			fmt.Sprintf("insider scan: %d finding(s) at severity %s (≥ --fail-on %s)",
				len(scan.Findings), scan.HighestSeverity(), failOn)).
			WithSuggestion(&output.Suggestion{
				Human: "review the findings in the body; investigate the audit event chain for each finding's event_ids",
			})
	}
	return nil
}

// parseFailOnSeverity normalises the --fail-on flag.  "none" turns the
// severity gate off entirely.
func parseFailOnSeverity(s string) (insider.Severity, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none", "off":
		return "", nil
	case "info":
		return insider.SeverityInfo, nil
	case "notice":
		return insider.SeverityNotice, nil
	case "warning", "":
		return insider.SeverityWarning, nil
	case "critical":
		return insider.SeverityCritical, nil
	}
	return "", fmt.Errorf("unknown severity %q", s)
}

func shouldFail(have, want insider.Severity) bool {
	if want == "" {
		return false
	}
	rank := map[insider.Severity]int{
		insider.SeverityInfo:     1,
		insider.SeverityNotice:   2,
		insider.SeverityWarning:  3,
		insider.SeverityCritical: 4,
	}
	return rank[have] >= rank[want]
}

// ----- list -----

func newInsiderListCmd() *cobra.Command {
	var (
		repoURL  string
		since    string
		minSev   string
		tenant   string
		findings bool
	)
	c := &cobra.Command{
		Use:          "list",
		Short:        "List insider scans newest-first",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInsiderList(cmd, repoURL, since, minSev, tenant, findings)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&since, "since", "",
		"only scans started at/after this RFC3339 timestamp")
	c.Flags().StringVar(&minSev, "min-severity", "",
		"only scans whose highest finding ≥ this severity")
	c.Flags().StringVar(&tenant, "tenant", "",
		"only scans scoped to this tenant")
	c.Flags().BoolVar(&findings, "with-findings", false,
		"only scans that have at least one finding")
	return c
}

func runInsiderList(cmd *cobra.Command, repoURL, since, minSev, tenant string, findings bool) error {
	d := DispatcherFrom(cmd)
	filter := insider.ListFilter{
		Tenant:          tenant,
		HasFindingsOnly: findings,
	}
	if since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return output.NewError("usage.bad_flag",
				fmt.Sprintf("insider list: --since: %v", err)).Wrap(output.ErrUsage)
		}
		filter.Since = &t
	}
	if minSev != "" {
		switch strings.ToLower(minSev) {
		case "info":
			filter.MinSeverity = insider.SeverityInfo
		case "notice":
			filter.MinSeverity = insider.SeverityNotice
		case "warning":
			filter.MinSeverity = insider.SeverityWarning
		case "critical":
			filter.MinSeverity = insider.SeverityCritical
		default:
			return output.NewError("usage.bad_flag",
				fmt.Sprintf("insider list: unknown --min-severity %q", minSev)).Wrap(output.ErrUsage)
		}
	}
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	scans, err := insider.NewScanStore(sp).List(cmd.Context(), filter)
	if err != nil {
		return output.NewError("insider.list_failed",
			fmt.Sprintf("insider list: %v", err)).Wrap(err)
	}
	body := insiderListBody{Count: len(scans)}
	for _, s := range scans {
		body.Entries = append(body.Entries, insiderScanSummary{
			ID:              s.ID,
			StartedAt:       s.StartedAt,
			Tenant:          s.Tenant,
			Findings:        len(s.Findings),
			HighestSeverity: string(s.HighestSeverity()),
			BaselineEvents:  s.BaselineEvents,
			TargetEvents:    s.TargetEvents,
		})
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// ----- show -----

func newInsiderShowCmd() *cobra.Command {
	var repoURL string
	c := &cobra.Command{
		Use:          "show <id>",
		Short:        "Show one scan's full body + findings",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInsiderShow(cmd, repoURL, args[0])
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	return c
}

func runInsiderShow(cmd *cobra.Command, repoURL, id string) error {
	d := DispatcherFrom(cmd)
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	scan, err := insider.NewScanStore(sp).Get(cmd.Context(), id)
	if err != nil {
		if errors.Is(err, insider.ErrScanNotFound) {
			return output.NewError("notfound.scan",
				fmt.Sprintf("insider show: scan %q not found", id)).Wrap(err)
		}
		return output.NewError("insider.get_failed",
			fmt.Sprintf("insider show: %v", err)).Wrap(err)
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(insiderScanBody{Scan: scan}))
}

// ----- bodies + text rendering -----

type insiderScanBody struct {
	*insider.Scan
}

// WriteText renders the scan verdict and per-finding rollup as human-readable
// text to w.
func (b insiderScanBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	s := b.Scan
	verdict := "✓ no findings"
	if hs := s.HighestSeverity(); hs != "" {
		verdict = "✗ findings — highest severity " + string(hs)
	}
	fmt.Fprintf(bw, "Insider scan %s\n", s.ID)
	fmt.Fprintf(bw, "  Verdict:        %s\n", verdict)
	if s.Tenant != "" {
		fmt.Fprintf(bw, "  Tenant:         %s\n", s.Tenant)
	}
	if s.Note != "" {
		fmt.Fprintf(bw, "  Note:           %s\n", s.Note)
	}
	fmt.Fprintf(bw, "  Baseline:       %s → %s (%d events, %d actors)\n",
		s.BaselineFrom.Format(time.RFC3339), s.BaselineTo.Format(time.RFC3339),
		s.BaselineEvents, s.BaselineActors)
	fmt.Fprintf(bw, "  Target:         %s → %s (%d events, %d actors)\n",
		s.TargetFrom.Format(time.RFC3339), s.TargetTo.Format(time.RFC3339),
		s.TargetEvents, s.TargetActors)
	fmt.Fprintf(bw, "  Spike factor:   %.2f\n", s.VolumeSpikeFactor)
	if len(s.Findings) == 0 {
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	fmt.Fprintln(bw)
	fmt.Fprintf(bw, "Findings (%d):\n", len(s.Findings))
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  SEVERITY\tTYPE\tACTOR\tACTION\tREASON")
	for _, f := range s.Findings {
		mark := "·"
		switch f.Severity {
		case insider.SeverityCritical:
			mark = "✗✗"
		case insider.SeverityWarning:
			mark = "✗"
		case insider.SeverityNotice:
			mark = "i"
		}
		fmt.Fprintf(tw, "  %s %s\t%s\t%s\t%s\t%s\n",
			mark, f.Severity, f.Type, f.Actor, f.Action, f.Reason)
	}
	_ = tw.Flush()
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type insiderScanSummary struct {
	ID              string    `json:"id"`
	StartedAt       time.Time `json:"started_at"`
	Tenant          string    `json:"tenant,omitempty"`
	Findings        int       `json:"findings"`
	HighestSeverity string    `json:"highest_severity,omitempty"`
	BaselineEvents  int       `json:"baseline_events"`
	TargetEvents    int       `json:"target_events"`
}

type insiderListBody struct {
	Count   int                  `json:"count"`
	Entries []insiderScanSummary `json:"entries"`
}

// WriteText renders the saved-scan list as a tabular summary to w.
func (b insiderListBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "%d scan(s)\n\n", b.Count)
	if b.Count == 0 {
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTARTED\tTENANT\tFINDINGS\tHIGHEST\tBASELINE/TARGET")
	for _, e := range b.Entries {
		highest := e.HighestSeverity
		if highest == "" {
			highest = "—"
		}
		tenant := e.Tenant
		if tenant == "" {
			tenant = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%d / %d\n",
			e.ID, e.StartedAt.Format(time.RFC3339), tenant,
			e.Findings, highest,
			e.BaselineEvents, e.TargetEvents)
	}
	_ = tw.Flush()
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// _ anchors the encoding/json import.
var _ = stdjson.Marshal
