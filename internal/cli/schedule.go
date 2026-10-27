// schedule.go — CLI surface for showing and editing per-deployment task schedules.
package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/schedule"
)

// newScheduleCmd implements `pg_hardstorage schedule [<deployment>
// [<expression>]]`. With no args, prints every configured
// deployment's schedules side-by-side — the at-a-glance "what's
// running when across the fleet?" view, useful for scheduling
// avoidance (ensuring backups don't all hit the same I/O peak).
//
// With one positional, prints the deployment's current schedule
// for the selected --task. With two positionals, sets it.
//
// Accepted expressions match what the agent's config consumes
// directly, plus a few wizard-friendly shorthand forms parsed via
// init.go's parseSchedExpr:
//
//	every 6h
//	every 30m
//	daily_at 04:00
//	at 2026-04-28T09:00:00Z
//	off                 (clears the schedule)
func newScheduleCmd() *cobra.Command {
	var task string
	c := &cobra.Command{
		Use:   "schedule [<deployment> [<expression>]]",
		Short: "Set, show, or list deployment backup/rotate schedules",
		Long: `schedule configures and inspects per-deployment
backup and rotate schedules in pg_hardstorage.yaml.

  pg_hardstorage schedule                                 # list every deployment's schedule (fleet view)
  pg_hardstorage schedule db1                             # show db1's --task schedule (default: backup)
  pg_hardstorage schedule db1 "every 6h"                  # set db1's backup schedule
  pg_hardstorage schedule db1 "daily_at 04:00" --task=rotate
  pg_hardstorage schedule db1 off                         # clear db1's --task schedule

The fleet listing surfaces both the backup and rotate schedule per
deployment — useful for avoiding I/O collisions ("did I schedule
two databases to back up at the same minute?") and for spotting
deployments without a schedule configured.`,
		Args:         cobra.RangeArgs(0, 2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch len(args) {
			case 0:
				return runScheduleList(cmd)
			case 1:
				return runScheduleShow(cmd, args[0], task)
			default:
				return runScheduleSet(cmd, args[0], task, strings.TrimSpace(args[1]))
			}
		},
	}
	c.Flags().StringVar(&task, "task", "backup",
		"which task's schedule to inspect/modify: backup | rotate | drill")
	return c
}

// runScheduleList walks every deployment in the loaded config and
// emits one row per (deployment, task) pair — backup, drill, and
// rotate, even when one or both are unset. An unset slot shows
// "off" so an operator immediately sees deployments without a
// schedule.
//
// Read-only; no config write. Sorted by deployment name so the
// fleet view is deterministic across runs.
func runScheduleList(cmd *cobra.Command) error {
	d := DispatcherFrom(cmd)
	p, cfg, _, err := loadEditableConfig()
	if err != nil {
		return err
	}
	var drift *scheduleDrift
	if rec := readRunningAgent(p.State.Value); rec != nil {
		drift = &scheduleDrift{rec: rec}
	}
	names := make([]string, 0, len(cfg.Deployments))
	for name := range cfg.Deployments {
		names = append(names, name)
	}
	sort.Strings(names)

	rows := make([]scheduleListRow, 0, 2*len(names))
	for _, name := range names {
		dep := cfg.Deployments[name]
		for _, task := range []string{"backup", "rotate", "drill"} {
			spec, _ := selectScheduleSpec(dep, task)
			row := scheduleListRow{
				Deployment: name,
				Task:       task,
				Spec:       spec,
			}
			if !spec.IsZero() {
				row.Description = describeSpec(spec)
			}
			if drift != nil && drift.stale(name, task, spec) {
				row.PendingRestart = true
				row.AgentRunning = describeRunning(drift.running(name, task))
			}
			rows = append(rows, row)
		}
	}
	body := scheduleListBody{Schedules: rows}
	if drift != nil {
		body.Agent = newScheduleAgentInfo(drift, cfg, rows)
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// scheduleListBody is the v1-stable result shape for the
// fleet-wide listing. Schema is additive over the
// per-deployment scheduleBody — a new sibling type so the two
// commands' bodies don't share `Updated` semantics that don't
// apply to read-only listing.
type scheduleListBody struct {
	Schedules []scheduleListRow `json:"schedules"`
	// Agent is present when an agent is running on this host. The rows show
	// the CONFIGURED schedule; the agent only applies a changed one after a
	// restart, and Agent says whether that is pending.
	Agent *scheduleAgentInfo `json:"agent,omitempty"`
}

type scheduleListRow struct {
	Deployment  string              `json:"deployment"`
	Task        string              `json:"task"`
	Spec        config.ScheduleSpec `json:"spec"`
	Description string              `json:"description,omitempty"`
	// PendingRestart is true when the running agent loaded a different
	// schedule for this slot than the configured one; AgentRunning is what
	// it is actually running ("off" = not scheduled).
	PendingRestart bool   `json:"pending_restart,omitempty"`
	AgentRunning   string `json:"agent_running,omitempty"`
}

// scheduleAgentInfo describes the running agent a schedule view was
// compared against.
type scheduleAgentInfo struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	// RestartRequired: at least one configured schedule differs from what
	// the agent loaded (including deployments removed from the config).
	RestartRequired bool `json:"restart_required"`
	// RemovedDeployments are still scheduled by the agent but no longer
	// configured.
	RemovedDeployments []string `json:"removed_deployments,omitempty"`
}

func newScheduleAgentInfo(drift *scheduleDrift, cfg *config.Config, rows []scheduleListRow) *scheduleAgentInfo {
	info := &scheduleAgentInfo{
		PID: drift.rec.PID, StartedAt: drift.rec.StartedAt,
		RemovedDeployments: drift.removedDeployments(cfg),
	}
	info.RestartRequired = len(info.RemovedDeployments) > 0
	for _, r := range rows {
		if r.PendingRestart {
			info.RestartRequired = true
		}
	}
	return info
}

// describeRunning renders what the agent runs for a slot.
func describeRunning(spec config.ScheduleSpec) string {
	if spec.IsZero() {
		return "off"
	}
	return describeSpec(spec)
}

// restartNote is the one-line explanation printed wherever a configured
// schedule differs from the running agent's.
func (a *scheduleAgentInfo) restartNote() string {
	return fmt.Sprintf("the running agent (pid %d, started %s) loaded its schedules at start and applies "+
		"changes only after a restart: restart it (e.g. systemctl restart pg_hardstorage-agent)",
		a.PID, a.StartedAt.Local().Format("2006-01-02 15:04:05"))
}

// WriteText renders a tabular fleet view. Deployments without a
// schedule get an explicit "off" row per task so the operator
// notices the gap.
func (b scheduleListBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if len(b.Schedules) == 0 {
		fmt.Fprintln(bw, "No deployments configured.")
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	// Count distinct deployments — never divide by a per-deployment
	// row constant (that broke silently when the drill task added a
	// third row per deployment).
	seen := make(map[string]struct{}, len(b.Schedules))
	for _, r := range b.Schedules {
		seen[r.Deployment] = struct{}{}
	}
	fmt.Fprintf(bw, "Schedules for %d deployment(s):\n", len(seen))
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  DEPLOYMENT\tTASK\tWHEN")
	for _, r := range b.Schedules {
		desc := r.Description
		if desc == "" {
			desc = "off"
		}
		if r.PendingRestart {
			desc += "   <-- NOT RUNNING YET: agent still runs " + r.AgentRunning
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.Deployment, r.Task, desc)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if b.Agent != nil && b.Agent.RestartRequired {
		fmt.Fprintf(bw, "\nNote: %s.\n", b.Agent.restartNote())
		if len(b.Agent.RemovedDeployments) > 0 {
			fmt.Fprintf(bw, "The agent still schedules removed deployment(s): %s.\n",
				strings.Join(b.Agent.RemovedDeployments, ", "))
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

func runScheduleShow(cmd *cobra.Command, deployment, task string) error {
	d := DispatcherFrom(cmd)
	p, cfg, _, err := loadEditableConfig()
	if err != nil {
		return err
	}
	dep, err := mustHaveDeployment(cfg, deployment)
	if err != nil {
		return err
	}
	spec, err := selectScheduleSpec(dep, task)
	if err != nil {
		return err
	}
	body := scheduleBody{
		Deployment: deployment,
		Task:       task,
		Spec:       spec,
	}
	if !spec.IsZero() {
		body.Description = describeSpec(spec)
	}
	body.noteAgent(readRunningAgent(p.State.Value), deployment, task, spec)
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

func runScheduleSet(cmd *cobra.Command, deployment, task, expr string) error {
	d := DispatcherFrom(cmd)

	spec := parseSchedExpr(expr) // shared with init wizard
	// Validate via the schedule parser unless the operator is
	// explicitly clearing — empty Spec is the "off" state.
	if !spec.IsZero() {
		if _, err := schedule.Parse(schedule.Spec(spec)); err != nil {
			return output.NewError("usage.bad_schedule",
				fmt.Sprintf("schedule: %v", err)).Wrap(output.ErrUsage)
		}
	}

	p, cfg, write, err := loadEditableConfig()
	if err != nil {
		return err
	}
	dep, err := mustHaveDeployment(cfg, deployment)
	if err != nil {
		return err
	}
	if err := setScheduleSpec(&dep, task, spec); err != nil {
		return err
	}
	cfg.Deployments[deployment] = dep
	if err := write(cfg); err != nil {
		return err
	}
	body := scheduleBody{
		Deployment: deployment,
		Task:       task,
		Spec:       spec,
		Updated:    true,
	}
	if !spec.IsZero() {
		body.Description = describeSpec(spec)
	}
	body.noteAgent(readRunningAgent(p.State.Value), deployment, task, spec)
	if body.Agent == nil {
		// No running agent was found on this host (it may run elsewhere or
		// under a supervisor with another state dir): the edit is still
		// applied only when an agent (re)starts.
		body.RestartNote = "the schedule applies when the agent (re)starts; a running agent keeps the schedule it loaded at start"
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// selectScheduleSpec returns the right ScheduleSpec field on the
// DeploymentConfig given the --task flag.
func selectScheduleSpec(dep config.DeploymentConfig, task string) (config.ScheduleSpec, error) {
	switch task {
	case "backup":
		return dep.Schedule.Backup, nil
	case "rotate":
		return dep.Schedule.Rotate, nil
	case "drill":
		return dep.Schedule.Drill, nil
	default:
		return config.ScheduleSpec{}, output.NewError("usage.bad_task",
			fmt.Sprintf("schedule: unknown --task %q (allowed: backup, rotate, drill)", task)).
			Wrap(output.ErrUsage)
	}
}

// setScheduleSpec mutates the right field. Mirror of selectScheduleSpec
// for the write path.
func setScheduleSpec(dep *config.DeploymentConfig, task string, spec config.ScheduleSpec) error {
	switch task {
	case "backup":
		dep.Schedule.Backup = spec
	case "rotate":
		dep.Schedule.Rotate = spec
	case "drill":
		dep.Schedule.Drill = spec
	default:
		return output.NewError("usage.bad_task",
			fmt.Sprintf("schedule: unknown --task %q (allowed: backup, rotate, drill)", task)).
			Wrap(output.ErrUsage)
	}
	return nil
}

// describeSpec renders a one-line human-readable form of a
// ScheduleSpec. Used in the result body for status output.
func describeSpec(s config.ScheduleSpec) string {
	parsed, err := schedule.Parse(schedule.Spec(s))
	if err != nil {
		return fmt.Sprintf("invalid: %v", err)
	}
	return parsed.Description()
}

type scheduleBody struct {
	Deployment  string              `json:"deployment"`
	Task        string              `json:"task"`
	Spec        config.ScheduleSpec `json:"spec"`
	Description string              `json:"description,omitempty"`
	Updated     bool                `json:"updated,omitempty"`
	// Agent, PendingRestart and AgentRunning are set when an agent is
	// running on this host and its loaded schedule for this slot differs
	// from Spec: Spec is what is CONFIGURED, AgentRunning what is running.
	Agent          *scheduleAgentInfo `json:"agent,omitempty"`
	PendingRestart bool               `json:"pending_restart,omitempty"`
	AgentRunning   string             `json:"agent_running,omitempty"`
	RestartNote    string             `json:"restart_note,omitempty"`
}

// noteAgent compares spec with the running agent's loaded schedule.
func (b *scheduleBody) noteAgent(rec *agentLoadedRecord, deployment, task string, spec config.ScheduleSpec) {
	if rec == nil {
		return
	}
	drift := &scheduleDrift{rec: rec}
	b.Agent = &scheduleAgentInfo{PID: rec.PID, StartedAt: rec.StartedAt}
	if drift.stale(deployment, task, spec) {
		b.PendingRestart = true
		b.AgentRunning = describeRunning(drift.running(deployment, task))
		b.Agent.RestartRequired = true
	}
}

// WriteText renders the task schedule as human-readable text to w, noting
// whether the value was just updated.
func (b scheduleBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	verb := "Schedule"
	if b.Updated {
		verb = "✓ Schedule updated"
	}
	fmt.Fprintf(bw, "%s for %s.%s:\n", verb, b.Deployment, b.Task)
	if b.Spec.IsZero() {
		fmt.Fprintln(bw, "  off")
	} else {
		if b.Description != "" {
			fmt.Fprintf(bw, "  %s\n", b.Description)
		}
		if b.Spec.Every != "" {
			fmt.Fprintf(bw, "  every:    %s\n", b.Spec.Every)
		}
		if b.Spec.DailyAt != "" {
			fmt.Fprintf(bw, "  daily_at: %s\n", b.Spec.DailyAt)
		}
		if b.Spec.At != "" {
			fmt.Fprintf(bw, "  at:       %s\n", b.Spec.At)
		}
	}
	if b.PendingRestart && b.Agent != nil {
		fmt.Fprintf(bw, "  NOT RUNNING YET: the agent still runs %s.\n  Note: %s.\n", b.AgentRunning, b.Agent.restartNote())
	} else if b.RestartNote != "" {
		fmt.Fprintf(bw, "  Note: %s.\n", b.RestartNote)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
