// schedule.go — CLI surface for showing and editing per-deployment task schedules.
package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

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
		"which task's schedule to inspect/modify: backup | rotate")
	return c
}

// runScheduleList walks every deployment in the loaded config and
// emits one row per (deployment, task) pair — both backup and
// rotate, even when one or both are unset. An unset slot shows
// "off" so an operator immediately sees deployments without a
// schedule.
//
// Read-only; no config write. Sorted by deployment name so the
// fleet view is deterministic across runs.
func runScheduleList(cmd *cobra.Command) error {
	d := DispatcherFrom(cmd)
	_, cfg, _, err := loadEditableConfig()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(cfg.Deployments))
	for name := range cfg.Deployments {
		names = append(names, name)
	}
	sort.Strings(names)

	rows := make([]scheduleListRow, 0, 2*len(names))
	for _, name := range names {
		dep := cfg.Deployments[name]
		for _, task := range []string{"backup", "rotate"} {
			spec, _ := selectScheduleSpec(dep, task)
			row := scheduleListRow{
				Deployment: name,
				Task:       task,
				Spec:       spec,
			}
			if !spec.IsZero() {
				row.Description = describeSpec(spec)
			}
			rows = append(rows, row)
		}
	}
	body := scheduleListBody{Schedules: rows}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// scheduleListBody is the v1-stable result shape for the
// fleet-wide listing. Schema is additive over the
// per-deployment scheduleBody — a new sibling type so the two
// commands' bodies don't share `Updated` semantics that don't
// apply to read-only listing.
type scheduleListBody struct {
	Schedules []scheduleListRow `json:"schedules"`
}

type scheduleListRow struct {
	Deployment  string              `json:"deployment"`
	Task        string              `json:"task"`
	Spec        config.ScheduleSpec `json:"spec"`
	Description string              `json:"description,omitempty"`
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
	fmt.Fprintf(bw, "Schedules for %d deployment(s):\n", len(b.Schedules)/2)
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  DEPLOYMENT\tTASK\tWHEN")
	for _, r := range b.Schedules {
		desc := r.Description
		if desc == "" {
			desc = "off"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.Deployment, r.Task, desc)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

func runScheduleShow(cmd *cobra.Command, deployment, task string) error {
	d := DispatcherFrom(cmd)
	_, cfg, _, err := loadEditableConfig()
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

	_, cfg, write, err := loadEditableConfig()
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
	default:
		return config.ScheduleSpec{}, output.NewError("usage.bad_task",
			fmt.Sprintf("schedule: unknown --task %q (allowed: backup, rotate)", task)).
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
	default:
		return output.NewError("usage.bad_task",
			fmt.Sprintf("schedule: unknown --task %q (allowed: backup, rotate)", task)).
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
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
