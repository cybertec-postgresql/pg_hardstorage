// timetravel.go — CLI surface for point-in-time recovery sessions (create/list/destroy/cleanup).
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/timetravel"
)

// newRealTimeTravelCmd implements `pg_hardstorage timetravel
// <create|list|destroy|cleanup>`.
//
// Timetravel sits next to standby in the lifecycle hierarchy: a
// standby follows production indefinitely; a timetravel session
// is pinned at a moment with a TTL. Both share the underlying
// restore + recovery plumbing — they differ only in
// Recovery.StandbyMode (standby) vs Recovery.Action="pause" with a
// concrete TargetTime/TargetLSN (timetravel).
func newRealTimeTravelCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "timetravel <create|list|destroy|cleanup>",
		Short: "Spin up an ephemeral read-only PG at a historical state",
		Long: `Manage ephemeral, time-pinned read-only PostgreSQL instances.

A timetravel session is a configured PGDATA at a specific historical
point — restore + recovery_target_time/lsn + recovery_action=pause.
PG comes up, replays WAL up to the target, pauses, and stays
readable. The session is tracked in a state file with a TTL so
forgotten sessions are reapable via 'timetravel cleanup'.

The data dir lives at --target; the operator starts PG separately
(systemd, pg_ctl, container — same posture as 'standby create').`,
	}
	c.AddCommand(
		newTimeTravelCreateCmd(),
		newTimeTravelListCmd(),
		newTimeTravelDestroyCmd(),
		newTimeTravelCleanupCmd(),
	)
	return c
}

func newTimeTravelCreateCmd() *cobra.Command {
	var (
		repoURL    string
		deployment string
		target     string
		at         string
		ttl        time.Duration
		force      bool
	)
	c := &cobra.Command{
		Use:   "create <name>",
		Short: "Provision a timetravel session pinned at --at",
		Long: `Restore the deployment's most recent backup whose stop time is
at-or-before --at into --target, configure recovery to replay up
to --at and pause, and record the session.

--at accepts:
  - RFC3339 timestamps:     "2026-04-01T00:00:00Z"
  - Natural language:        "5 minutes ago", "yesterday 9pm"
  - PG LSNs:                 "0/3F5A1B40"

The session expires after --ttl (default 1h). Run 'timetravel
cleanup' periodically (or wire it to cron) to reap expired
sessions.

After 'timetravel create' returns, start PG separately:

  pg_ctl -D <target> start

PG comes up paused at the recovery target. Promote with
pg_promote() if (and only if) you actually want a writable copy
that diverges from production.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTimeTravelCreate(cmd, timeTravelCreateOptions{
				name:       args[0],
				repoURL:    repoURL,
				deployment: deployment,
				target:     target,
				at:         at,
				ttl:        ttl,
				force:      force,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	c.Flags().StringVar(&deployment, "deployment", "", "source deployment (required)")
	c.Flags().StringVar(&target, "target", "", "target data directory (required)")
	c.Flags().StringVar(&at, "at", "", "target time/LSN (RFC3339, natural language, or LSN; required)")
	c.Flags().DurationVar(&ttl, "ttl", timetravel.DefaultTTL, "session expiry; cleanup will reap past this")
	c.Flags().BoolVar(&force, "force", false, "permit a non-empty --target")
	return c
}

type timeTravelCreateOptions struct {
	name       string
	repoURL    string
	deployment string
	target     string
	at         string
	ttl        time.Duration
	force      bool
}

func runTimeTravelCreate(cmd *cobra.Command, opts timeTravelCreateOptions) error {
	d := DispatcherFrom(cmd)
	for _, missing := range []struct{ name, val string }{
		{"--repo", opts.repoURL}, {"--deployment", opts.deployment},
		{"--target", opts.target}, {"--at", opts.at},
	} {
		if missing.val == "" {
			return output.NewError("usage.missing_flag",
				fmt.Sprintf("timetravel create: %s is required", missing.name)).Wrap(output.ErrUsage)
		}
	}

	verifier, err := loadVerifier()
	if err != nil {
		return err
	}
	mgr, err := timeTravelManager()
	if err != nil {
		return err
	}

	res, err := mgr.Create(cmd.Context(), timetravel.CreateOptions{
		Name:           opts.name,
		Deployment:     opts.deployment,
		RepoURL:        opts.repoURL,
		TargetDir:      opts.target,
		At:             opts.at,
		TTL:            opts.ttl,
		Verifier:       verifier,
		KEKForRef:      resolveKEKForVerify, // shared with verify --full / standby
		UnwrapDEK:      resolveDEKForVerify, // cloud-KMS DEK unwrap (issue #102)
		AllowOverwrite: opts.force,
	})
	if err != nil {
		return mapTimeTravelError("timetravel create", err)
	}
	binPath, _ := os.Executable()
	body := timeTravelCreateBody{
		Session:    res,
		StartCmd:   fmt.Sprintf("pg_ctl -D %s start", res.TargetDir),
		FetchUsing: binPath,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

func newTimeTravelListCmd() *cobra.Command {
	var includeExpired bool
	c := &cobra.Command{
		Use:          "list",
		Short:        "List timetravel sessions (active by default)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			mgr, err := timeTravelManager()
			if err != nil {
				return err
			}
			out, err := mgr.List(includeExpired)
			if err != nil {
				return output.NewError("timetravel.list_failed",
					fmt.Sprintf("timetravel list: %v", err)).Wrap(err)
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(timeTravelListBody{Sessions: out}))
		},
	}
	c.Flags().BoolVar(&includeExpired, "include-expired", false,
		"also list sessions past their TTL")
	return c
}

func newTimeTravelDestroyCmd() *cobra.Command {
	var removeTarget bool
	c := &cobra.Command{
		Use:          "destroy <name>",
		Short:        "Remove a session from the state file (data dir kept by default)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := DispatcherFrom(cmd)
			mgr, err := timeTravelManager()
			if err != nil {
				return err
			}
			if err := mgr.Destroy(cmd.Context(), args[0], timetravel.DestroyOptions{
				RemoveTargetDir: removeTarget,
			}); err != nil {
				return mapTimeTravelError("timetravel destroy", err)
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(timeTravelDestroyBody{
				Name:          args[0],
				RemovedTarget: removeTarget,
			}))
		},
	}
	c.Flags().BoolVar(&removeTarget, "remove-target", false,
		"also rm -rf the data directory")
	return c
}

func newTimeTravelCleanupCmd() *cobra.Command {
	var removeTargets bool
	c := &cobra.Command{
		Use:          "cleanup",
		Short:        "Reap expired timetravel sessions",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			mgr, err := timeTravelManager()
			if err != nil {
				return err
			}
			res, err := mgr.Cleanup(cmd.Context(), removeTargets)
			if err != nil {
				return mapTimeTravelError("timetravel cleanup", err)
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(timeTravelCleanupBody{
				Reaped:          res.Reaped,
				RemainingActive: res.RemainingActive,
				RemovedTargets:  removeTargets,
			}))
		},
	}
	c.Flags().BoolVar(&removeTargets, "remove-targets", false,
		"also rm -rf each reaped session's data dir")
	return c
}

// --- helpers ---------------------------------------------------------

func timeTravelManager() (*timetravel.Manager, error) {
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return nil, output.NewError("paths.resolve_failed",
			fmt.Sprintf("timetravel: resolve paths: %v", err)).Wrap(err)
	}
	state := filepath.Join(p.State.String(), "timetravel.json")
	bin, _ := os.Executable()
	if bin == "" {
		bin = "pg_hardstorage"
	}
	return timetravel.NewManager(state, bin), nil
}

func mapTimeTravelError(op string, err error) error {
	switch {
	case errors.Is(err, timetravel.ErrAlreadyExists):
		return output.NewError("conflict.timetravel_exists",
			fmt.Sprintf("%s: %v", op, err)).Wrap(err)
	case errors.Is(err, timetravel.ErrNotFound):
		return output.NewError("notfound.timetravel",
			fmt.Sprintf("%s: %v", op, err)).Wrap(err)
	}
	return output.NewError("timetravel.failed",
		fmt.Sprintf("%s: %v", op, err)).Wrap(err)
}

// --- bodies ----------------------------------------------------------

type timeTravelCreateBody struct {
	Session    *timetravel.Session `json:"session"`
	StartCmd   string              `json:"start_cmd"`
	FetchUsing string              `json:"fetch_using"`
}

// WriteText renders the provisioned timetravel session — recovery target and
// follow-up start command — as human-readable text to w.
func (b timeTravelCreateBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ timetravel %s provisioned\n", b.Session.Name)
	fmt.Fprintf(bw, "  Deployment:   %s\n", b.Session.Deployment)
	fmt.Fprintf(bw, "  Backup:       %s\n", b.Session.BackupID)
	fmt.Fprintf(bw, "  Target:       %s\n", b.Session.TargetDir)
	if !b.Session.TargetTime.IsZero() {
		fmt.Fprintf(bw, "  Recovery to:  %s\n", b.Session.TargetTime.Format(time.RFC3339))
	}
	if b.Session.TargetLSN != "" {
		fmt.Fprintf(bw, "  Recovery to:  LSN %s\n", b.Session.TargetLSN)
	}
	fmt.Fprintf(bw, "  Expires at:   %s\n", b.Session.ExpiresAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "\nNext step — start PG:\n  %s", b.StartCmd)
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type timeTravelListBody struct {
	Sessions []timetravel.Session `json:"sessions"`
}

// WriteText renders the timetravel session list as a tabular summary to w,
// flagging expired sessions.
func (b timeTravelListBody) WriteText(w io.Writer) error {
	if len(b.Sessions) == 0 {
		_, err := io.WriteString(w, "no timetravel sessions")
		return err
	}
	bw := &strings.Builder{}
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tDEPLOYMENT\tBACKUP\tTARGET-AT\tEXPIRES")
	for _, s := range b.Sessions {
		at := s.TargetSpec
		if at == "" && !s.TargetTime.IsZero() {
			at = s.TargetTime.Format(time.RFC3339)
		}
		state := "active"
		if s.IsExpired() {
			state = "expired"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s (%s)\n",
			s.Name, s.Deployment, s.BackupID, at,
			s.ExpiresAt.Format(time.RFC3339), state)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type timeTravelDestroyBody struct {
	Name          string `json:"name"`
	RemovedTarget bool   `json:"removed_target"`
}

// WriteText renders the destroy confirmation, noting whether the data dir
// was removed, as a single-line summary to w.
func (b timeTravelDestroyBody) WriteText(w io.Writer) error {
	state := "(data dir kept)"
	if b.RemovedTarget {
		state = "(data dir removed)"
	}
	_, err := fmt.Fprintf(w, "✓ timetravel %s destroyed %s", b.Name, state)
	return err
}

type timeTravelCleanupBody struct {
	Reaped          []string `json:"reaped"`
	RemainingActive int      `json:"remaining_active"`
	RemovedTargets  bool     `json:"removed_targets"`
}

// WriteText renders the cleanup outcome — reaped names plus active count —
// as human-readable text to w.
func (b timeTravelCleanupBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ timetravel cleanup\n")
	fmt.Fprintf(bw, "  Reaped:        %d\n", len(b.Reaped))
	if len(b.Reaped) > 0 {
		fmt.Fprintf(bw, "    %s\n", strings.Join(b.Reaped, ", "))
	}
	fmt.Fprintf(bw, "  Active:        %d\n", b.RemainingActive)
	if b.RemovedTargets {
		fmt.Fprintf(bw, "  Removed dirs:  yes\n")
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
