// recovery_drill_history.go — CLI surface for listing past recovery drill outcomes.
package cli

import (
	stdjson "encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/recovery"
)

// newRecoveryDrillHistoryCmd implements `recovery drill history`.
//
// Read-only surface over the auto-persisted drill history.  Each
// `recovery drill` run records a slim DrillHistoryEntry into
// recovery/drills/<id>.json; this command lists + summarises
// them for trend analysis.
//
// Operationally answers:
//
//   - "How often does this deployment's drill pass?"  →  pass_percent
//   - "Is the verdict trend improving / stable / regressing?"
//   - "What's the RTO actual distribution across runs?"
//     → min / median / mean / max
//   - "When was the last drill?  What was the verdict?"
func newRecoveryDrillHistoryCmd() *cobra.Command {
	var (
		repoURL    string
		deployment string
		verdict    string
		since      string
		until      string
		limit      int
		reverse    bool
		format     string
		summarize  bool
	)
	c := &cobra.Command{
		Use:   "history [<deployment>]",
		Short: "List + summarise past drill runs (auto-recorded by `recovery drill`)",
		Long: `Read-only surface over the auto-persisted drill history.

Each ` + "`recovery drill`" + ` run writes a slim DrillHistoryEntry into
` + "`recovery/drills/<id>.json`" + ` in the repo (suppress with
` + "`--skip-history`" + `).  This command walks them and reports the
list + the rollup summary (pass percent, RTO distribution,
verdict trend, latest verdict + RTO).

Filtering:
  <deployment>          (positional) restrict to one deployment
  --verdict pass|partial|fail   restrict by verdict
  --since DURATION_OR_RFC3339   lower-bound on generated_at
  --until DURATION_OR_RFC3339   upper-bound on generated_at
  --limit N                     cap returned entries (0 = all)
  --reverse                     newest-first ordering

Output:
  --format json     (default) — JSON body, the v1 contract
  --format markdown — forensics-grade GFM rendering with the
                      trend table + per-entry table
  --summarize       suppress the per-entry slice; just the
                    summary rollup (cheaper output for
                    dashboards)

Read-only; safe at any cadence including against WORM-locked
repos.`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if deployment != "" && deployment != args[0] {
					return output.NewError("usage.bad_flag",
						"recovery drill history: positional deployment + --deployment disagree").
						Wrap(output.ErrUsage)
				}
				deployment = args[0]
			}
			return runRecoveryDrillHistory(cmd, recoveryDrillHistoryFlags{
				repoURL:    repoURL,
				deployment: deployment,
				verdict:    verdict,
				since:      since,
				until:      until,
				limit:      limit,
				reverse:    reverse,
				format:     format,
				summarize:  summarize,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&deployment, "deployment", "",
		"restrict to one deployment (positional <deployment> also accepted)")
	c.Flags().StringVar(&verdict, "verdict", "",
		"restrict by verdict: pass | partial | fail")
	c.Flags().StringVar(&since, "since", "",
		"lower-bound on generated_at (e.g. 7d, 30d, RFC3339)")
	c.Flags().StringVar(&until, "until", "",
		"upper-bound on generated_at (RFC3339)")
	c.Flags().IntVar(&limit, "limit", 0, "cap returned entries (0 = all)")
	c.Flags().BoolVar(&reverse, "reverse", true, "newest-first ordering (default: true)")
	c.Flags().StringVar(&format, "format", "json",
		"output format: json | markdown")
	c.Flags().BoolVar(&summarize, "summarize", false,
		"suppress the per-entry slice; just the summary rollup")
	return c
}

type recoveryDrillHistoryFlags struct {
	repoURL    string
	deployment string
	verdict    string
	since      string
	until      string
	limit      int
	reverse    bool
	format     string
	summarize  bool
}

func runRecoveryDrillHistory(cmd *cobra.Command, f recoveryDrillHistoryFlags) error {
	d := DispatcherFrom(cmd)
	switch f.format {
	case "", "json", "markdown":
	default:
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("recovery drill history: --format must be json or markdown; got %q", f.format)).
			Wrap(output.ErrUsage)
	}
	if f.limit < 0 {
		return output.NewError("usage.bad_flag",
			"recovery drill history: --limit must be >= 0").Wrap(output.ErrUsage)
	}

	var verdict recovery.DrillVerdict
	switch strings.ToLower(f.verdict) {
	case "":
	case "pass":
		verdict = recovery.DrillVerdictPass
	case "partial":
		verdict = recovery.DrillVerdictPartial
	case "fail":
		verdict = recovery.DrillVerdictFail
	default:
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("recovery drill history: --verdict must be pass|partial|fail; got %q", f.verdict)).
			Wrap(output.ErrUsage)
	}

	since, err := parseSinceUntil(f.since)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("recovery drill history: --since: %v", err)).Wrap(output.ErrUsage)
	}
	until, err := parseSinceUntil(f.until)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("recovery drill history: --until: %v", err)).Wrap(output.ErrUsage)
	}

	_, sp, err := openRepo(cmd.Context(), f.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	store := recovery.NewHistoryStore(sp)
	entries, err := store.List(cmd.Context(), recovery.HistoryFilter{
		Deployment: f.deployment,
		Verdict:    verdict,
		Since:      since,
		Until:      until,
		Limit:      f.limit,
		Reverse:    f.reverse,
	})
	if err != nil {
		return output.NewError("recovery.drill_history_failed",
			fmt.Sprintf("recovery drill history: %v", err)).Wrap(err)
	}

	// Compute the summary using time-ordered entries (oldest
	// first); reverse the slice locally if the caller asked
	// for newest-first display.
	timeOrder := append([]*recovery.DrillHistoryEntry(nil), entries...)
	sort.Slice(timeOrder, func(i, j int) bool {
		return timeOrder[i].GeneratedAt.Before(timeOrder[j].GeneratedAt)
	})
	summary := recovery.Summarize(timeOrder)

	body := drillHistoryBody{
		URL:        f.repoURL,
		Deployment: f.deployment,
		Verdict:    f.verdict,
		Summary:    summary,
		Entries:    entries,
		summarize:  f.summarize,
		format:     f.format,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// drillHistoryBody wraps the list + summary for the dual JSON /
// Markdown output flow.  JSON shape:
//
//	{ "schema": "pg_hardstorage.recovery.drill_history.v1",
//	  "url": "...", "deployment": "...", "verdict": "pass",
//	  "summary": {...}, "entries": [...] }
//
// With --summarize, "entries" is omitted.
type drillHistoryBody struct {
	URL        string                        `json:"url"`
	Deployment string                        `json:"deployment,omitempty"`
	Verdict    string                        `json:"verdict,omitempty"`
	Summary    *recovery.HistorySummary      `json:"summary"`
	Entries    []*recovery.DrillHistoryEntry `json:"entries,omitempty"`

	summarize bool
	format    string
}

// MarshalJSON emits either the summarised or full history view depending on
// whether the operator requested a rollup-only response.
func (b drillHistoryBody) MarshalJSON() ([]byte, error) {
	type alias drillHistoryBody
	if b.summarize {
		return stdjson.Marshal(struct {
			URL        string                   `json:"url"`
			Deployment string                   `json:"deployment,omitempty"`
			Verdict    string                   `json:"verdict,omitempty"`
			Summary    *recovery.HistorySummary `json:"summary"`
		}{b.URL, b.Deployment, b.Verdict, b.Summary})
	}
	return stdjson.Marshal(alias(b))
}

// WriteText renders the drill history to w, choosing markdown when format is
// "markdown" and the compact summary otherwise.
func (b drillHistoryBody) WriteText(w io.Writer) error {
	if strings.EqualFold(b.format, "markdown") {
		return writeDrillHistoryMarkdown(w, b)
	}
	return writeDrillHistoryCompact(w, b)
}

// writeDrillHistoryMarkdown renders a forensics-grade GFM
// document with the summary table + per-entry table.
func writeDrillHistoryMarkdown(w io.Writer, b drillHistoryBody) error {
	bw := &strings.Builder{}
	scope := "all deployments"
	if b.Deployment != "" {
		scope = "deployment `" + b.Deployment + "`"
	}
	fmt.Fprintf(bw, "# pg_hardstorage drill history — %s\n\n", scope)
	fmt.Fprintln(bw, "| Field | Value |")
	fmt.Fprintln(bw, "| --- | --- |")
	fmt.Fprintf(bw, "| Repository | `%s` |\n", b.URL)
	if b.Deployment != "" {
		fmt.Fprintf(bw, "| Deployment | `%s` |\n", b.Deployment)
	}
	if b.Verdict != "" {
		fmt.Fprintf(bw, "| Verdict filter | `%s` |\n", b.Verdict)
	}
	fmt.Fprintln(bw)

	// Summary section.
	fmt.Fprintln(bw, "## Summary")
	fmt.Fprintln(bw)
	s := b.Summary
	if s == nil || s.Total == 0 {
		fmt.Fprintln(bw, "_No drills recorded for the requested scope._")
		fmt.Fprintln(bw)
	} else {
		trendIcon := "·"
		switch s.VerdictTrend {
		case "improving":
			trendIcon = "✓"
		case "regressing":
			trendIcon = "✗"
		}
		fmt.Fprintln(bw, "| Metric | Value |")
		fmt.Fprintln(bw, "| --- | --- |")
		fmt.Fprintf(bw, "| Total runs | %d |\n", s.Total)
		fmt.Fprintf(bw, "| Pass / Partial / Fail | %d / %d / %d |\n",
			s.PassCount, s.PartialCount, s.FailCount)
		fmt.Fprintf(bw, "| Pass percent | %.2f%% |\n", s.PassPercent)
		if s.RTOMaxSeconds > 0 {
			fmt.Fprintf(bw, "| RTO actual (min / median / mean / max) | %ds / %ds / %ds / %ds |\n",
				s.RTOMinSeconds, s.RTOMedianSeconds, s.RTOMeanSeconds, s.RTOMaxSeconds)
		}
		if !s.LatestAt.IsZero() {
			fmt.Fprintf(bw, "| Latest run | %s |\n", s.LatestAt.Format(time.RFC3339))
			fmt.Fprintf(bw, "| Latest verdict | %s |\n", s.LatestVerdict)
			if s.LatestRTO > 0 {
				fmt.Fprintf(bw, "| Latest RTO actual | %ds |\n", s.LatestRTO)
			}
		}
		if s.VerdictTrend != "" {
			fmt.Fprintf(bw, "| Verdict trend | %s %s |\n", trendIcon, s.VerdictTrend)
		}
		fmt.Fprintln(bw)
	}

	// Per-entry table (suppressed in --summarize).
	if !b.summarize {
		fmt.Fprintln(bw, "## Drills (newest first)")
		fmt.Fprintln(bw)
		if len(b.Entries) == 0 {
			fmt.Fprintln(bw, "_(no drills)_")
			fmt.Fprintln(bw)
		} else {
			fmt.Fprintln(bw, "| When | Deployment | Backup | Verdict | RTO actual | Issues | Operator |")
			fmt.Fprintln(bw, "| --- | --- | --- | --- | --- | --- | --- |")
			for _, e := range b.Entries {
				ic := drillVerdictIcon(e.Verdict)
				rto := "—"
				if e.RTOActualSeconds > 0 {
					rto = fmt.Sprintf("%ds", e.RTOActualSeconds)
				}
				op := e.Operator
				if op == "" {
					op = "—"
				}
				issues := "—"
				if e.IssueCount > 0 {
					issues = fmt.Sprintf("%d (crit %d)", e.IssueCount, e.CriticalCount)
				}
				fmt.Fprintf(bw, "| %s | `%s` | `%s` | %s %s | %s | %s | %s |\n",
					e.GeneratedAt.Format(time.RFC3339),
					e.Deployment, e.BackupID,
					ic, e.Verdict, rto, issues, op)
			}
			fmt.Fprintln(bw)
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n")+"\n")
	return err
}

// writeDrillHistoryCompact renders a single-screen overview for
// `-o text` without --format markdown.
func writeDrillHistoryCompact(w io.Writer, b drillHistoryBody) error {
	bw := &strings.Builder{}
	scope := "all deployments"
	if b.Deployment != "" {
		scope = "deployment " + b.Deployment
	}
	fmt.Fprintf(bw, "drill history — %s — %s\n\n", b.URL, scope)
	s := b.Summary
	if s == nil || s.Total == 0 {
		fmt.Fprintln(bw, "(no drills recorded)")
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	fmt.Fprintf(bw, "Total runs:         %d\n", s.Total)
	fmt.Fprintf(bw, "Pass / Part / Fail: %d / %d / %d\n",
		s.PassCount, s.PartialCount, s.FailCount)
	fmt.Fprintf(bw, "Pass percent:       %.2f%%\n", s.PassPercent)
	if s.RTOMaxSeconds > 0 {
		fmt.Fprintf(bw, "RTO actual:         min %ds / median %ds / mean %ds / max %ds\n",
			s.RTOMinSeconds, s.RTOMedianSeconds, s.RTOMeanSeconds, s.RTOMaxSeconds)
	}
	if s.VerdictTrend != "" {
		fmt.Fprintf(bw, "Trend:              %s\n", s.VerdictTrend)
	}
	if !s.LatestAt.IsZero() {
		fmt.Fprintf(bw, "Latest:             %s — %s",
			s.LatestAt.Format("2006-01-02T15:04:05Z"), s.LatestVerdict)
		if s.LatestRTO > 0 {
			fmt.Fprintf(bw, " (RTO %ds)", s.LatestRTO)
		}
		fmt.Fprintln(bw)
	}

	if !b.summarize && len(b.Entries) > 0 {
		fmt.Fprintln(bw)
		tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "WHEN\tDEPLOYMENT\tVERDICT\tRTO\tISSUES")
		for _, e := range b.Entries {
			rto := "-"
			if e.RTOActualSeconds > 0 {
				rto = fmt.Sprintf("%ds", e.RTOActualSeconds)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s %s\t%s\t%d\n",
				e.GeneratedAt.Format("2006-01-02T15:04:05Z"),
				e.Deployment,
				drillVerdictIcon(e.Verdict), e.Verdict,
				rto, e.IssueCount)
		}
		_ = tw.Flush()
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// drillVerdictIcon mirrors the markdown.go helper.  Inlined here
// to avoid adding a public surface to the recovery package.
func drillVerdictIcon(v recovery.DrillVerdict) string {
	switch v {
	case recovery.DrillVerdictPass:
		return "✓"
	case recovery.DrillVerdictPartial:
		return "·"
	default:
		return "✗"
	}
}
