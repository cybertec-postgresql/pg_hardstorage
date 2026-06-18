// gameday.go — CLI surface for chaos / disaster-recovery exercises (list, run, report).
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/gameday"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// newRealGameDayCmd implements `pg_hardstorage gameday <list|run|report>`.
//
// v0.1 ships the registry-driven shape: scenarios self-register in
// internal/gameday/scenarios.go, and the CLI walks the registry. The
// scenarios themselves document their invariants and (in v0.1)
// pass-by-contract; runtime drive of the fault injection lands when
// the supervisor's child-control surface and the storage plugin's
// fault-injection middleware are exposed.
//
// The CLI shape is locked so an operator wiring `gameday run
// agent_kill` into a quarterly cron job today gets the same
// invocation when lands and the scenarios start actually
// killing processes.
func newRealGameDayCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "gameday <list|run|report>",
		Short: "Opt-in chaos automation",
	}
	c.AddCommand(
		newGameDayListCmd(),
		newGameDayRunCmd(),
		newGameDayReportCmd(),
	)
	return c
}

func newGameDayListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered scenarios",
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			scenarios := gameday.List()
			body := gameDayListBody{}
			for _, s := range scenarios {
				body.Scenarios = append(body.Scenarios, gameDayListEntry{
					Name:        s.Name,
					Tier:        s.Tier,
					Description: s.Description,
				})
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
		},
	}
}

func newGameDayRunCmd() *cobra.Command {
	var (
		deployment    string
		repoURL       string
		recoverWithin time.Duration
		faultDuration time.Duration
		dryRun        bool
	)
	c := &cobra.Command{
		Use:   "run <scenario>",
		Short: "Run one chaos scenario, return structured Pass/Fail",
		Long: `Run one chaos scenario from the registry.

v0.1 scenarios document the invariant they assert and (when run with
--dry-run) the planned fault injection; runtime drive of the kill
signal / 503-storm / Patroni switchover lands alongside the
supervisor's child-control surface and the storage plugin's
fault-injection middleware.

The CLI shape is locked: an operator wiring 'gameday run agent_kill'
into a quarterly cron today gets the same invocation when lands.

Use 'gameday list' to see registered scenarios.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGameDayRun(cmd, args[0], gameday.RunOptions{
				Deployment:    deployment,
				RepoURL:       repoURL,
				RecoverWithin: recoverWithin,
				FaultDuration: faultDuration,
				DryRun:        dryRun,
			})
		},
	}
	c.Flags().StringVar(&deployment, "deployment", "",
		"deployment to target (scenario-dependent)")
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL (scenario-dependent)")
	c.Flags().DurationVar(&recoverWithin, "recover-within", 0,
		"upper bound for recovery (scenario default applies if 0)")
	c.Flags().DurationVar(&faultDuration, "fault-duration", 0,
		"how long the fault is held active (scenario default applies if 0)")
	c.Flags().BoolVar(&dryRun, "dry-run", false,
		"print the planned action; don't inject the fault")
	return c
}

func runGameDayRun(cmd *cobra.Command, name string, opts gameday.RunOptions) error {
	d := DispatcherFrom(cmd)
	res, err := gameday.Run(cmd.Context(), name, opts)
	if err != nil {
		if errors.Is(err, gameday.ErrNoSuchScenario) {
			scenarios := gameday.List()
			names := make([]string, len(scenarios))
			for i, s := range scenarios {
				names[i] = s.Name
			}
			return output.NewError("notfound.scenario",
				fmt.Sprintf("gameday run: scenario %q not registered", name)).
				WithSuggestion(&output.Suggestion{
					Human: fmt.Sprintf("registered: %s", strings.Join(names, ", ")),
				}).Wrap(output.ErrUsage)
		}
		return output.NewError("gameday.run_failed",
			fmt.Sprintf("gameday run: %v", err)).Wrap(err)
	}

	// Plan principle: "Each simulation produces a report (recovered /
	// RTO actual / alerts fired) attached to the audit log." Best-
	// effort emission — a missing --repo skips audit (the operator
	// asked for an ad-hoc run); a real audit-store failure logs but
	// doesn't change the verdict (the JSON result already carries the
	// evidence). Dry-runs DO emit so the ledger reflects "we
	// rehearsed at this time."
	if opts.RepoURL != "" {
		emitGameDayAudit(cmd.Context(), opts.RepoURL, name, res)
	}

	body := gameDayRunBody{Result: res}
	if !res.Pass {
		// Surface a verify.failed-style error so the exit code reflects
		// the run not passing. Body still lands as the structured
		// payload so a JSON consumer sees the evidence list.
		err := output.NewError("verify.failed",
			fmt.Sprintf("gameday run %s: %s", name, res.Failure))
		_ = d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
		return err
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// emitGameDayAudit appends a `gameday.run` audit event to the chain
// at repoURL. Failure to open the repo or append is swallowed —
// the JSON Result is the authoritative output and the operator
// already has it; the audit chain is the "later, when we want to
// answer 'when did we last rehearse this?', it's there" record.
func emitGameDayAudit(ctx context.Context, repoURL, scenario string, res *gameday.Result) {
	repoMeta, sp, err := openRepo(ctx, repoURL)
	if err != nil {
		return
	}
	defer sp.Close()
	store := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	body := map[string]any{
		"scenario":    scenario,
		"pass":        res.Pass,
		"dry_run":     res.DryRun,
		"started_at":  res.StartedAt,
		"stopped_at":  res.StoppedAt,
		"duration_ms": res.Duration.Milliseconds(),
		"recovery_ms": res.RecoveryTime.Milliseconds(),
		"failure":     res.Failure,
		"evidence":    res.Evidence,
	}
	store.AppendOrLog(ctx, &audit.Event{
		Action:    "gameday.run",
		Subject:   audit.Subject{Repo: repoURL},
		Timestamp: time.Now().UTC(),
		Body:      body,
	})
}

// --- gameday report --------------------------------------------------

func newGameDayReportCmd() *cobra.Command {
	var (
		repoURL  string
		scenario string
		since    time.Duration
		limit    int
	)
	c := &cobra.Command{
		Use:   "report",
		Short: "Aggregate recent gameday runs from the audit chain",
		Long: `Walks the audit chain at --repo for ` + "`gameday.run`" + `
events and reports per-scenario pass/fail counts plus the most-
recent run timestamp. Useful for the "did our quarterly chaos
plan land?" question regulators ask.

Default window is 90 days (--since 90d-equivalent); pass --since
0 to include the entire chain. --scenario X filters to a single
scenario. --limit caps the per-scenario detail list.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runGameDayReport(cmd, repoURL, scenario, since, limit)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL whose audit chain to walk (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&scenario, "scenario", "",
		"only report on this scenario (default: every gameday.run event)")
	c.Flags().DurationVar(&since, "since", 90*24*time.Hour,
		"only include events at-or-after now-since (default 90d; pass 0 for unbounded)")
	c.Flags().IntVar(&limit, "limit", 50,
		"max events to include in the per-scenario detail list (0 = all)")
	return c
}

func runGameDayReport(cmd *cobra.Command, repoURL, scenarioFilter string, since time.Duration, limit int) error {
	d := DispatcherFrom(cmd)
	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	store := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	filters := audit.ListFilters{
		Action: "gameday.run",
	}
	if since > 0 {
		filters.Since = time.Now().UTC().Add(-since)
	}
	events, err := store.Search(cmd.Context(), filters)
	if err != nil {
		return output.NewError("gameday.report_search_failed",
			fmt.Sprintf("gameday report: %v", err)).Wrap(err)
	}

	// Aggregate by scenario.
	type aggCounters struct {
		Total          int
		Passes         int
		Fails          int
		DryRuns        int
		MostRecentAt   time.Time
		MostRecentPass bool
	}
	by := map[string]*aggCounters{}
	var detail []gameDayReportRow

	for _, ev := range events {
		scenName, _ := ev.Body["scenario"].(string)
		if scenName == "" {
			scenName = "<unknown>"
		}
		if scenarioFilter != "" && scenName != scenarioFilter {
			continue
		}
		pass, _ := ev.Body["pass"].(bool)
		dryRun, _ := ev.Body["dry_run"].(bool)
		dur, _ := ev.Body["duration_ms"].(float64)
		failure, _ := ev.Body["failure"].(string)

		ag := by[scenName]
		if ag == nil {
			ag = &aggCounters{}
			by[scenName] = ag
		}
		ag.Total++
		if dryRun {
			ag.DryRuns++
		}
		if pass {
			ag.Passes++
		} else {
			ag.Fails++
		}
		if ev.Timestamp.After(ag.MostRecentAt) {
			ag.MostRecentAt = ev.Timestamp
			ag.MostRecentPass = pass
		}
		if limit == 0 || len(detail) < limit {
			detail = append(detail, gameDayReportRow{
				Scenario:   scenName,
				Pass:       pass,
				DryRun:     dryRun,
				At:         ev.Timestamp,
				DurationMS: int64(dur),
				Failure:    failure,
				EventID:    ev.ID,
				Sequence:   ev.Sequence,
			})
		}
	}

	// Build per-scenario summaries sorted by name for deterministic output.
	summaries := make([]gameDayReportSummary, 0, len(by))
	scenNames := make([]string, 0, len(by))
	for name := range by {
		scenNames = append(scenNames, name)
	}
	sort.Strings(scenNames)
	for _, name := range scenNames {
		ag := by[name]
		summaries = append(summaries, gameDayReportSummary{
			Scenario:       name,
			Total:          ag.Total,
			Passes:         ag.Passes,
			Fails:          ag.Fails,
			DryRuns:        ag.DryRuns,
			MostRecentAt:   ag.MostRecentAt,
			MostRecentPass: ag.MostRecentPass,
		})
	}

	body := gameDayReportBody{
		Schema:        "pg_hardstorage.gameday.report.v1",
		Repo:          repoURL,
		Filter:        scenarioFilter,
		WindowSeconds: int64(since / time.Second),
		Total:         len(events),
		Scenarios:     summaries,
		Events:        detail,
		GeneratedAt:   time.Now().UTC(),
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// --- bodies / writers -----------------------------------------------

type gameDayListBody struct {
	Scenarios []gameDayListEntry `json:"scenarios"`
}

type gameDayListEntry struct {
	Name        string `json:"name"`
	Tier        string `json:"tier"`
	Description string `json:"description"`
}

// WriteText renders the registered scenarios as a tabular summary to w.
func (b gameDayListBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintln(bw, "registered gameday scenarios:")
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAME\tTIER\tDESCRIPTION")
	for _, e := range b.Scenarios {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", e.Name, e.Tier, e.Description)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type gameDayRunBody struct {
	*gameday.Result
}

// MarshalJSON emits the embedded gameday.Result so the JSON contract stays
// the domain v1 shape.
func (b gameDayRunBody) MarshalJSON() ([]byte, error) {
	return json.MarshalIndent(b.Result, "", "  ")
}

// WriteText renders the gameday run outcome, including any recorded evidence
// events, as human-readable text to w.
func (b gameDayRunBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	verb := "✓"
	state := "PASS"
	if !b.Pass {
		verb = "✗"
		state = "FAIL"
	}
	if b.DryRun {
		state += " (dry-run)"
	}
	fmt.Fprintf(bw, "%s gameday run %s — %s\n", verb, b.Scenario, state)
	fmt.Fprintf(bw, "  Duration:    %s\n", b.Duration)
	if b.RecoveryTime > 0 {
		fmt.Fprintf(bw, "  Recovery:    ≤ %s\n", b.RecoveryTime)
	}
	if b.Failure != "" {
		fmt.Fprintf(bw, "  Failure:     %s\n", b.Failure)
	}
	if len(b.Evidence) > 0 {
		fmt.Fprintln(bw, "  Evidence:")
		for _, e := range b.Evidence {
			fmt.Fprintf(bw, "    [%s] %s — %s\n", e.At.Format(time.RFC3339), e.Kind, e.Message)
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// --- gameday report bodies ------------------------------------------

type gameDayReportSummary struct {
	Scenario       string    `json:"scenario"`
	Total          int       `json:"total"`
	Passes         int       `json:"passes"`
	Fails          int       `json:"fails"`
	DryRuns        int       `json:"dry_runs"`
	MostRecentAt   time.Time `json:"most_recent_at"`
	MostRecentPass bool      `json:"most_recent_pass"`
}

type gameDayReportRow struct {
	Scenario   string    `json:"scenario"`
	Pass       bool      `json:"pass"`
	DryRun     bool      `json:"dry_run,omitempty"`
	At         time.Time `json:"at"`
	DurationMS int64     `json:"duration_ms"`
	Failure    string    `json:"failure,omitempty"`
	EventID    string    `json:"event_id,omitempty"`
	Sequence   int64     `json:"sequence,omitempty"`
}

type gameDayReportBody struct {
	Schema        string                 `json:"schema"`
	Repo          string                 `json:"repo,omitempty"`
	Filter        string                 `json:"scenario_filter,omitempty"`
	WindowSeconds int64                  `json:"window_seconds"`
	Total         int                    `json:"total_events"`
	Scenarios     []gameDayReportSummary `json:"scenarios"`
	Events        []gameDayReportRow     `json:"events,omitempty"`
	GeneratedAt   time.Time              `json:"generated_at"`
}

// WriteText renders the gameday history rollup — per-scenario counters plus
// the most-recent event line — as human-readable text to w.
func (b gameDayReportBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if b.WindowSeconds > 0 {
		fmt.Fprintf(bw, "gameday report — last %s\n",
			time.Duration(b.WindowSeconds)*time.Second)
	} else {
		fmt.Fprintln(bw, "gameday report — full chain")
	}
	if b.Filter != "" {
		fmt.Fprintf(bw, "  Filter:        scenario=%s\n", b.Filter)
	}
	fmt.Fprintf(bw, "  Total events:  %d\n", b.Total)
	if len(b.Scenarios) == 0 {
		fmt.Fprintln(bw, "  no gameday runs in window")
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	fmt.Fprintln(bw, "")
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  SCENARIO\tRUNS\tPASS\tFAIL\tDRY-RUN\tMOST-RECENT\tLAST")
	for _, s := range b.Scenarios {
		recent := "—"
		if !s.MostRecentAt.IsZero() {
			recent = s.MostRecentAt.UTC().Format(time.RFC3339)
		}
		last := "—"
		if !s.MostRecentAt.IsZero() {
			if s.MostRecentPass {
				last = "✓"
			} else {
				last = "✗"
			}
		}
		fmt.Fprintf(tw, "  %s\t%d\t%d\t%d\t%d\t%s\t%s\n",
			s.Scenario, s.Total, s.Passes, s.Fails, s.DryRuns, recent, last)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
