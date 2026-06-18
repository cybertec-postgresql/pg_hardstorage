// patroni.go — CLI surface for Patroni REST interactions (status, follow, history).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/patroni"
)

// newPatroniCmd implements `pg_hardstorage patroni <status|history>`.
//
// The plan calls Patroni's REST surface out as the operationally-
// correct path for leader-follow + slot-continuity.+ ships
// the read-side first: `patroni status` (cluster topology + leader
// + lag) and `patroni history` (timeline-history events). The
// agent-side leader-follow loop builds on these primitives in the
// next session.
//
// Operator-facing shape:
//
//	pg_hardstorage patroni status --url http://patroni:8008
//	pg_hardstorage patroni history --url http://patroni:8008
func newPatroniCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "patroni <status|history>",
		Short: "Read-only view of a Patroni-managed PostgreSQL cluster",
		Long: `Patroni REST integration.

` + "`status`" + `  prints the cluster topology — leader, replicas, lag,
         per-member timeline. Useful as a doctor-style sanity
         check before kicking off a backup or restore against a
         Patroni cluster.

` + "`history`" + ` prints the timeline-history events Patroni records on
         every promotion. This is what the agent's leader-follow
         loop will consume to capture <new_tli>.history files into
         the repo (next session).

Authentication: Patroni's REST is HTTP basic-auth or no auth.
Read endpoints are typically open; pass --user/--password if your
deployment locks them down.`,
	}
	c.AddCommand(newPatroniStatusCmd())
	c.AddCommand(newPatroniHistoryCmd())
	c.AddCommand(newPatroniFollowCmd())
	return c
}

func newPatroniStatusCmd() *cobra.Command {
	var (
		url      string
		user     string
		password string
	)
	c := &cobra.Command{
		Use:          "status",
		Short:        "Print the Patroni cluster topology + leader",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPatroniStatus(cmd, url, user, password)
		},
	}
	c.Flags().StringVar(&url, "url", "",
		"Patroni REST base URL (e.g. http://patroni-leader:8008) — required")
	_ = c.MarkFlagRequired("url")
	c.Flags().StringVar(&user, "user", "", "HTTP basic-auth username (optional)")
	c.Flags().StringVar(&password, "password", "", "HTTP basic-auth password (optional)")
	return c
}

func runPatroniStatus(cmd *cobra.Command, url, user, password string) error {
	d := DispatcherFrom(cmd)
	c, err := buildPatroniClient(url, user, password)
	if err != nil {
		return err
	}
	cluster, err := c.Cluster(cmd.Context())
	if err != nil {
		return mapPatroniError("status", err)
	}
	body := patroniStatusBody{
		Schema:  "pg_hardstorage.patroni.status.v1",
		Scope:   cluster.Scope,
		Members: cluster.Members,
	}
	for i := range cluster.Members {
		if cluster.Members[i].IsLeader() {
			body.LeaderName = cluster.Members[i].Name
			body.LeaderTimeline = cluster.Members[i].Timeline
		}
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

func newPatroniHistoryCmd() *cobra.Command {
	var (
		url      string
		user     string
		password string
	)
	c := &cobra.Command{
		Use:          "history",
		Short:        "Print Patroni's timeline-history events",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPatroniHistory(cmd, url, user, password)
		},
	}
	c.Flags().StringVar(&url, "url", "", "Patroni REST base URL — required")
	_ = c.MarkFlagRequired("url")
	c.Flags().StringVar(&user, "user", "", "HTTP basic-auth username (optional)")
	c.Flags().StringVar(&password, "password", "", "HTTP basic-auth password (optional)")
	return c
}

func runPatroniHistory(cmd *cobra.Command, url, user, password string) error {
	d := DispatcherFrom(cmd)
	c, err := buildPatroniClient(url, user, password)
	if err != nil {
		return err
	}
	events, err := c.History(cmd.Context())
	if err != nil {
		return mapPatroniError("history", err)
	}
	body := patroniHistoryBody{
		Schema: "pg_hardstorage.patroni.history.v1",
		Events: events,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// newPatroniFollowCmd implements `pg_hardstorage patroni follow`.
//
// The plan calls the agent's leader-follow loop out as the
// foundation of Patroni Mechanism 1: poll /cluster, observe
// leader changes, capture TIMELINE_HISTORY on every promotion,
// reconnect the WAL stream against the new leader.
//
// This command exposes the polling primitive so an operator can
// (a) verify their Patroni REST is reachable, (b) watch a live
// failover happen, (c) sanity-check what the agent will do when
// the same primitive is wired into `agent run` in a follow-up.
//
// Operator-facing shape:
//
//	pg_hardstorage patroni follow --url http://patroni:8008
//	pg_hardstorage patroni follow --url ... --interval 2s --duration 30s
//	pg_hardstorage patroni follow --url ... -o ndjson | jq    # streaming
//
// `--duration 0` runs forever until SIGINT (the default for
// production-style monitoring).
func newPatroniFollowCmd() *cobra.Command {
	var (
		url      string
		user     string
		password string
		interval time.Duration
		duration time.Duration
	)
	c := &cobra.Command{
		Use:   "follow",
		Short: "Watch a Patroni cluster and stream leader-change events",
		Long: `Polls /cluster at the configured interval and prints a
leader-change event each time the cluster's primary changes.
This is the operator-side mirror of the agent's internal
leader-follow loop (used by WAL streaming + backup runs to
reconnect on Patroni failover).

Output: text mode prints one line per change. NDJSON mode
streams structured events for piping into jq / log
aggregation. Initial leader observation is the first event.

Stops on SIGINT or when --duration elapses (0 = forever).`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPatroniFollow(cmd, url, user, password, interval, duration)
		},
	}
	c.Flags().StringVar(&url, "url", "", "Patroni REST base URL — required")
	_ = c.MarkFlagRequired("url")
	c.Flags().StringVar(&user, "user", "", "HTTP basic-auth username (optional)")
	c.Flags().StringVar(&password, "password", "", "HTTP basic-auth password (optional)")
	c.Flags().DurationVar(&interval, "interval", patroni.DefaultFollowInterval,
		"poll cadence (default 5s; matches Patroni's default leader TTL window)")
	c.Flags().DurationVar(&duration, "duration", 0,
		"how long to run; 0 = until SIGINT")
	return c
}

func runPatroniFollow(cmd *cobra.Command, url, user, password string, interval, duration time.Duration) error {
	d := DispatcherFrom(cmd)
	c, err := buildPatroniClient(url, user, password)
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	if duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}

	// Hand-off channel: the follower's OnEvent callback runs on
	// the poll goroutine and must return promptly. We push events
	// through a buffered channel and drain on the main goroutine
	// so the dispatcher's renderer (which may block on stdout) is
	// never inside the poll path.
	const eventBuf = 64
	events := make(chan patroni.LeaderChange, eventBuf)
	pollErrs := make(chan error, eventBuf)

	f, err := patroni.Start(ctx, patroni.FollowerOptions{
		Client:   c,
		Interval: interval,
		OnEvent: func(ev patroni.LeaderChange) {
			select {
			case events <- ev:
			default:
				// Buffer full: drop. The renderer is much slower
				// than realistic Patroni cadence, so this is only
				// reachable when stdout is wedged. We surface a
				// poll-error so the operator sees we're dropping.
				pollErrs <- fmt.Errorf("patroni follow: event buffer full, dropping change for %s", ev.New)
			}
		},
		OnPollError: func(e error) {
			select {
			case pollErrs <- e:
			default:
			}
		},
	})
	if err != nil {
		return output.NewError("patroni.failed",
			fmt.Sprintf("patroni follow: start: %v", err)).Wrap(err)
	}

	// Drain loop. Exits when ctx fires or the follower's Done
	// channel closes (which happens after ctx fires too — both are
	// the same termination signal in practice).
	for {
		select {
		case <-f.Done():
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(patroniFollowSummary{
				Schema:        "pg_hardstorage.patroni.follow.v1",
				ExitedCleanly: true,
			}))
		case ev := <-events:
			body := patroniLeaderChangeBody{
				Schema:    "pg_hardstorage.patroni.leader_change.v1",
				At:        ev.At,
				OldLeader: leaderEndpointJSON(ev.Old),
				NewLeader: leaderEndpointJSON(ev.New),
			}
			_ = d.Event(ctx, output.NewEvent(output.SeverityNotice, "patroni", "leader_change").
				WithBody(body))
		case err := <-pollErrs:
			_ = d.Event(ctx, output.NewEvent(output.SeverityWarning, "patroni", "poll_error").
				WithBody(map[string]any{"error": err.Error()}))
		}
	}
}

// patroniFollowSummary is the (small) result body emitted when
// `patroni follow` exits cleanly. Per-event detail is streamed
// through the event channel; this just signals end-of-run.
type patroniFollowSummary struct {
	Schema        string `json:"schema"`
	ExitedCleanly bool   `json:"exited_cleanly"`
}

// WriteText renders the follow-loop end-of-run summary as a single line to w.
func (b patroniFollowSummary) WriteText(w io.Writer) error {
	if b.ExitedCleanly {
		_, err := io.WriteString(w, "patroni follow: exited cleanly")
		return err
	}
	_, err := io.WriteString(w, "patroni follow: exited")
	return err
}

// patroniLeaderChangeBody is the per-event body for the leader-
// change events streamed by `patroni follow`.
type patroniLeaderChangeBody struct {
	Schema    string                 `json:"schema"`
	At        time.Time              `json:"at"`
	OldLeader *patroniLeaderEndpoint `json:"old_leader,omitempty"`
	NewLeader *patroniLeaderEndpoint `json:"new_leader,omitempty"`
}

// patroniLeaderEndpoint is the JSON shape of a leader endpoint
// in the streamed events. Decoupled from patroni.LeaderEndpoint so
// that internal field renames don't change the on-the-wire schema
// (24-month compat applies).
type patroniLeaderEndpoint struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Timeline uint32 `json:"timeline"`
	Role     string `json:"role"`
}

func leaderEndpointJSON(e *patroni.LeaderEndpoint) *patroniLeaderEndpoint {
	if e == nil {
		return nil
	}
	return &patroniLeaderEndpoint{
		Name:     e.Name,
		Host:     e.Host,
		Port:     e.Port,
		Timeline: e.Timeline,
		Role:     e.Role,
	}
}

// buildPatroniClient is the shared construction path. Maps URL
// parse errors to a structured CLI error.
func buildPatroniClient(url, user, password string) (*patroni.Client, error) {
	opts := []patroni.ClientOption{}
	if user != "" {
		opts = append(opts, patroni.WithAuth(user, password))
	}
	c, err := patroni.NewClient(url, opts...)
	if err != nil {
		return nil, output.NewError("usage.bad_flag",
			fmt.Sprintf("patroni: --url: %v", err)).Wrap(output.ErrUsage)
	}
	return c, nil
}

// mapPatroniError translates the patroni package's sentinel errors
// to the project's structured CLI error codes.
func mapPatroniError(verb string, err error) error {
	switch {
	case errors.Is(err, patroni.ErrUnreachable):
		return output.NewError("storage.unreachable",
			fmt.Sprintf("patroni %s: %v", verb, err)).
			WithSuggestion(&output.Suggestion{
				Human: "check the --url is reachable and the Patroni REST endpoint is up (default port 8008)",
			}).Wrap(err)
	case errors.Is(err, patroni.ErrUnauthorized):
		return output.NewError("auth.denied",
			fmt.Sprintf("patroni %s: %v", verb, err)).
			WithSuggestion(&output.Suggestion{
				Human: "Patroni's REST endpoint requires authentication; pass --user / --password",
			}).Wrap(err)
	case errors.Is(err, patroni.ErrNoLeader):
		return output.NewError("notfound.leader",
			fmt.Sprintf("patroni %s: %v", verb, err)).
			WithSuggestion(&output.Suggestion{
				Human: "the cluster has no current leader (DCS lock not held). Likely a failover-in-progress; retry shortly.",
			}).Wrap(err)
	}
	return output.NewError("patroni.failed",
		fmt.Sprintf("patroni %s: %v", verb, err)).Wrap(err)
}

// patroniStatusBody is the v1-stable result body for `patroni
// status`.
type patroniStatusBody struct {
	Schema         string           `json:"schema"`
	Scope          string           `json:"scope,omitempty"`
	LeaderName     string           `json:"leader_name,omitempty"`
	LeaderTimeline uint32           `json:"leader_timeline,omitempty"`
	Members        []patroni.Member `json:"members"`
}

// WriteText renders the cluster status — leader plus per-member rollup — as
// human-readable text to w.
func (b patroniStatusBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if b.Scope != "" {
		fmt.Fprintf(bw, "patroni cluster %q\n", b.Scope)
	} else {
		fmt.Fprintln(bw, "patroni cluster")
	}
	if b.LeaderName != "" {
		fmt.Fprintf(bw, "  Leader: %s (TLI %d)\n", b.LeaderName, b.LeaderTimeline)
	} else {
		fmt.Fprintln(bw, "  ✗ no current leader")
	}
	fmt.Fprintln(bw)
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAME\tROLE\tSTATE\tHOST\tPORT\tTLI\tLAG")
	for _, m := range b.Members {
		lag := "-"
		if m.Lag != nil {
			lag = fmt.Sprintf("%d", *m.Lag)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			m.Name, m.Role, m.State, m.Host, m.Port, m.Timeline, lag)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// patroniHistoryBody is the v1-stable result body for `patroni
// history`.
type patroniHistoryBody struct {
	Schema string                 `json:"schema"`
	Events []patroni.HistoryEvent `json:"events"`
}

// WriteText renders the cluster promotion history as a tabular summary to w.
func (b patroniHistoryBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "patroni history — %d events\n", len(b.Events))
	if len(b.Events) == 0 {
		fmt.Fprintln(bw, "  (cluster has had no recorded promotions)")
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  TLI\tSWITCH-LSN\tNEW LEADER\tWHEN\tREASON")
	for _, e := range b.Events {
		when := "-"
		if !e.Timestamp.IsZero() {
			when = e.Timestamp.UTC().Format(time.RFC3339)
		}
		newLeader := "-"
		if e.NewLeader != "" {
			newLeader = e.NewLeader
		}
		reason := e.Reason
		if reason == "" {
			reason = "-"
		}
		fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\t%s\n",
			e.Timeline, e.SwitchLSN, newLeader, when, reason)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
