// deployment.go — CLI surface for managing the deployment catalogue (add/edit/remove/list/test).
package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

// newDeploymentCmd is the `deployment` command tree. Subcommands:
//
//	deployment list                     show every configured deployment
//	deployment add <name> --connection ... --repo ...
//	deployment remove <name>            (with --yes confirmation)
//	deployment edit <name>              update fields without re-typing the rest
//	deployment test <name>              probe PG, verify connection
//
// All write paths go through loadEditableConfig from configio.go.
// The `add` validator probes PG by default so a typo'd connection
// is caught at config time, not at first scheduled backup.
func newDeploymentCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "deployment",
		Short: "Manage deployments (add/list/remove/edit/test)",
		Long: `A deployment is the unit of "what we back up" — one PostgreSQL
service. The subcommands here mutate the deployments: block in
pg_hardstorage.yaml.`,
	}
	c.AddCommand(newDeploymentListCmd())
	c.AddCommand(newDeploymentAddCmd())
	c.AddCommand(newDeploymentRemoveCmd())
	c.AddCommand(newDeploymentEditCmd())
	c.AddCommand(newDeploymentTestCmd())
	return c
}

func newDeploymentListCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "list",
		Short:        "List configured deployments",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeploymentList(cmd)
		},
	}
}

func runDeploymentList(cmd *cobra.Command) error {
	d := DispatcherFrom(cmd)
	_, cfg, _, err := loadEditableConfig()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(cfg.Deployments))
	for k := range cfg.Deployments {
		names = append(names, k)
	}
	sort.Strings(names)

	out := make([]deploymentListEntry, 0, len(names))
	for _, n := range names {
		dep := cfg.Deployments[n]
		entry := deploymentListEntry{
			Name:           n,
			PGConnection:   redactDSN(dep.PGConnection),
			Repo:           dep.Repo,
			Tenant:         dep.Tenant,
			Classification: effectiveClassification(dep.Classification),
			BackupSchedule: scheduleSummary(dep.Schedule.Backup),
			RotateSchedule: scheduleSummary(dep.Schedule.Rotate),
		}
		if dep.Patroni.IsEnabled() {
			entry.Patroni = patroniSummary(dep.Patroni)
		}
		out = append(out, entry)
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(deploymentListBody{Deployments: out}))
}

// redactDSN masks password parameters in a libpq connection string.
// libpq accepts two forms; we redact both:
//
//   - URI form:     postgres://user:password@host/db?password=...
//   - Keyword form: host=db1 user=u password=secret dbname=app
//
// We never want `deployment list` output to leak credentials into a
// screenshot or a log paste.
func redactDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	// URI form: scheme://user:pass@host/...
	if i := strings.Index(dsn, "://"); i >= 0 {
		rest := dsn[i+3:]
		if at := strings.IndexByte(rest, '@'); at >= 0 {
			cred := rest[:at]
			if colon := strings.IndexByte(cred, ':'); colon >= 0 {
				rest = cred[:colon] + ":****" + rest[at:]
				dsn = dsn[:i+3] + rest
			}
		}
		// Also redact ?password=... in the URI's query string.
		dsn = redactKeywordPassword(dsn)
		return dsn
	}
	// Keyword form: scan for password= and replace its value.
	return redactKeywordPassword(dsn)
}

// redactKeywordPassword scans for "password=<value>" occurrences and
// replaces the value with ****. Handles both unquoted (`password=foo`,
// terminated by whitespace, &, or end-of-string) and single-quoted
// (`password='f o o'`) forms. Conservative — preserves position so the
// rest of the string still parses identically modulo the secret.
func redactKeywordPassword(s string) string {
	const tag = "password="
	out := s
	for i := 0; ; {
		idx := strings.Index(strings.ToLower(out[i:]), tag)
		if idx < 0 {
			return out
		}
		valStart := i + idx + len(tag)
		if valStart >= len(out) {
			return out
		}
		if out[valStart] == '\'' {
			// Single-quoted value — redact between quotes.
			end := strings.IndexByte(out[valStart+1:], '\'')
			if end < 0 {
				return out[:valStart+1] + "****" // unterminated
			}
			out = out[:valStart+1] + "****" + out[valStart+1+end:]
			i = valStart + 1 + 4 + 1 // past closing quote
			continue
		}
		// Unquoted value — terminated by space, & (URI query), or end.
		end := valStart
		for end < len(out) && out[end] != ' ' && out[end] != '\t' && out[end] != '&' {
			end++
		}
		out = out[:valStart] + "****" + out[end:]
		i = valStart + 4
	}
}

// patroniSummary projects a PatroniConfig to the list-rendering view.
// Only fields the operator wrote (or the implicit single-slot default)
// are surfaced; empty fields are omitted so a sparsely-configured block
// renders sparsely. Mirrors the operator's mental model: "show me what's
// in the YAML."
func patroniSummary(p config.PatroniConfig) *deploymentListPatroni {
	out := &deploymentListPatroni{
		URL:      p.URL,
		Slot:     p.Slot,
		Interval: p.Interval,
	}
	if len(p.Slots) > 0 {
		out.Slots = make([]deploymentListPatroniSlot, 0, len(p.Slots))
		for _, s := range p.Slots {
			out.Slots = append(out.Slots, deploymentListPatroniSlot{
				Name: s.Name, Role: s.Role,
			})
		}
	}
	if p.User != "" {
		out.User = p.User
	}
	return out
}

func scheduleSummary(s config.ScheduleSpec) string {
	if s.IsZero() {
		return "off"
	}
	switch {
	case s.Every != "":
		return "every " + s.Every
	case s.DailyAt != "":
		return "daily_at " + s.DailyAt
	case s.At != "":
		return "at " + s.At
	}
	return "?"
}

func newDeploymentAddCmd() *cobra.Command {
	var (
		conn        string
		repo        string
		tenant      string
		schedBackup string
		schedRotate string
		skipProbe   bool
		yes         bool
	)
	c := &cobra.Command{
		Use:          "add <name>",
		Short:        "Add a new deployment to the config",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeploymentAdd(cmd, args[0], deploymentAddOpts{
				connection: conn, repo: repo, tenant: tenant,
				schedBackup: schedBackup, schedRotate: schedRotate,
				skipProbe: skipProbe, yes: yes,
			})
		},
	}
	c.Flags().StringVar(&conn, "connection", "", "libpq connection string (required)")
	_ = c.MarkFlagRequired("connection")
	c.Flags().StringVar(&repo, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&tenant, "tenant", "", "tenant scope (default: default)")
	c.Flags().StringVar(&schedBackup, "schedule-backup", "every 6h", "backup schedule expression")
	c.Flags().StringVar(&schedRotate, "schedule-rotate", "daily_at 04:00", "rotate schedule expression")
	c.Flags().BoolVar(&skipProbe, "skip-probe", false,
		"don't connect to PG to validate the connection")
	c.Flags().BoolVar(&yes, "yes", false,
		"replace an existing deployment with the same name without confirmation")
	return c
}

type deploymentAddOpts struct {
	connection, repo, tenant string
	schedBackup, schedRotate string
	skipProbe, yes           bool
}

func runDeploymentAdd(cmd *cobra.Command, name string, opts deploymentAddOpts) error {
	d := DispatcherFrom(cmd)

	// Probe before we touch the config. A bad connection at config
	// time becomes a recurring scheduled-backup failure if we let
	// it through; better to surface immediately.
	if !opts.skipProbe {
		if err := probePG(cmd.Context(), opts.connection); err != nil {
			return output.NewError("deployment.probe_failed",
				fmt.Sprintf("deployment add: cannot reach PG: %v", err)).
				WithSuggestion(&output.Suggestion{
					Human: "verify the DSN, that the user has REPLICATION, and that pg_hba.conf permits replication from this host. --skip-probe disables this check.",
				}).Wrap(err)
		}
	}

	_, cfg, write, err := loadEditableConfig()
	if err != nil {
		return err
	}
	if cfg.Deployments == nil {
		cfg.Deployments = map[string]config.DeploymentConfig{}
	}
	if _, exists := cfg.Deployments[name]; exists && !opts.yes {
		return output.NewError("conflict.deployment_exists",
			fmt.Sprintf("deployment add: deployment %q already exists; pass --yes to replace", name)).
			WithSuggestion(&output.Suggestion{
				Human: "use `pg_hardstorage deployment edit " + name + "` to mutate fields in place",
			})
	}
	dep := config.DeploymentConfig{
		PGConnection: opts.connection,
		Repo:         opts.repo,
		Tenant:       opts.tenant,
		Schedule: config.DeploymentSchedule{
			Backup: parseSchedExpr(opts.schedBackup),
			Rotate: parseSchedExpr(opts.schedRotate),
		},
	}
	cfg.Deployments[name] = dep
	if err := write(cfg); err != nil {
		return err
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(deploymentMutationBody{
		Name: name, Action: "added",
	}))
}

func newDeploymentRemoveCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:          "remove <name>",
		Short:        "Remove a deployment from the config",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeploymentRemove(cmd, args[0], yes)
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation gate")
	return c
}

func runDeploymentRemove(cmd *cobra.Command, name string, yes bool) error {
	d := DispatcherFrom(cmd)
	_, cfg, write, err := loadEditableConfig()
	if err != nil {
		return err
	}
	if _, ok := cfg.Deployments[name]; !ok {
		return output.NewError("notfound.deployment",
			fmt.Sprintf("deployment remove: no such deployment %q", name))
	}
	if !yes {
		return output.NewError("aborted.confirmation_required",
			fmt.Sprintf("deployment remove: refusing to remove %q without --yes", name)).
			WithSuggestion(&output.Suggestion{
				Human: "this only removes config; existing backups in the repo are NOT touched. Re-run with --yes when you're sure.",
			})
	}
	delete(cfg.Deployments, name)
	if err := write(cfg); err != nil {
		return err
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(deploymentMutationBody{
		Name: name, Action: "removed",
	}))
}

func newDeploymentEditCmd() *cobra.Command {
	var (
		conn            string
		repo            string
		tenant          string
		schedBackup     string
		schedRotate     string
		patroniURL      string
		patroniSlot     string
		patroniInterval string
		skipProbe       bool
	)
	c := &cobra.Command{
		Use:          "edit <name>",
		Short:        "Update fields on an existing deployment",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		Long: `Updates only the fields the operator passes. Other fields
retain their current values. Pass --connection "" or --repo "" to
clear those fields explicitly.

Patroni fields (--patroni-url, --patroni-slot, --patroni-interval)
mutate the deployment's patroni: block. Passing --patroni-url ""
disables Patroni for this deployment. The multi-slot (Slots) form
is not editable via flags — use a YAML edit for that case.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeploymentEdit(cmd, args[0], deploymentEditOpts{
				connection: conn, repo: repo, tenant: tenant,
				schedBackup: schedBackup, schedRotate: schedRotate,
				patroniURL: patroniURL, patroniSlot: patroniSlot,
				patroniInterval: patroniInterval,
				skipProbe:       skipProbe,
				connectionSet:   cmd.Flags().Changed("connection"),
				repoSet:         cmd.Flags().Changed("repo"),
				tenantSet:       cmd.Flags().Changed("tenant"),
				backupSet:       cmd.Flags().Changed("schedule-backup"),
				rotateSet:       cmd.Flags().Changed("schedule-rotate"),
				patroniURLSet:   cmd.Flags().Changed("patroni-url"),
				patroniSlotSet:  cmd.Flags().Changed("patroni-slot"),
				patroniIntvlSet: cmd.Flags().Changed("patroni-interval"),
			})
		},
	}
	c.Flags().StringVar(&conn, "connection", "", "libpq connection string")
	c.Flags().StringVar(&repo, "repo", "", "repository URL")
	c.Flags().StringVar(&tenant, "tenant", "", "tenant scope")
	c.Flags().StringVar(&schedBackup, "schedule-backup", "", "backup schedule expression")
	c.Flags().StringVar(&schedRotate, "schedule-rotate", "", "rotate schedule expression")
	c.Flags().StringVar(&patroniURL, "patroni-url", "",
		`Patroni REST URL (e.g. http://patroni:8008); "" disables`)
	c.Flags().StringVar(&patroniSlot, "patroni-slot", "",
		"physical replication slot name (single-slot mode)")
	c.Flags().StringVar(&patroniInterval, "patroni-interval", "",
		"Patroni poll cadence (e.g. 5s)")
	c.Flags().BoolVar(&skipProbe, "skip-probe", false,
		"don't connect to PG to validate the new connection")
	return c
}

type deploymentEditOpts struct {
	connection, repo, tenant                 string
	schedBackup, schedRotate                 string
	patroniURL, patroniSlot, patroniInterval string
	skipProbe                                bool
	// *Set fields tell us whether the operator passed each flag —
	// we apply only those edits, leaving others as-is.
	connectionSet, repoSet, tenantSet, backupSet, rotateSet bool
	patroniURLSet, patroniSlotSet, patroniIntvlSet          bool
}

func runDeploymentEdit(cmd *cobra.Command, name string, opts deploymentEditOpts) error {
	d := DispatcherFrom(cmd)
	_, cfg, write, err := loadEditableConfig()
	if err != nil {
		return err
	}
	dep, err := mustHaveDeployment(cfg, name)
	if err != nil {
		return err
	}
	if opts.connectionSet {
		dep.PGConnection = opts.connection
	}
	if opts.repoSet {
		dep.Repo = opts.repo
	}
	if opts.tenantSet {
		dep.Tenant = opts.tenant
	}
	if opts.backupSet {
		dep.Schedule.Backup = parseSchedExpr(opts.schedBackup)
	}
	if opts.rotateSet {
		dep.Schedule.Rotate = parseSchedExpr(opts.schedRotate)
	}
	if opts.patroniURLSet {
		dep.Patroni.URL = opts.patroniURL
		// Clearing the URL disables Patroni for this deployment;
		// drop the rest of the block so YAML doesn't leave dangling
		// slot/interval entries that no longer mean anything.
		if dep.Patroni.URL == "" {
			dep.Patroni = config.PatroniConfig{}
		}
	}
	if opts.patroniSlotSet {
		dep.Patroni.Slot = opts.patroniSlot
	}
	if opts.patroniIntvlSet {
		dep.Patroni.Interval = opts.patroniInterval
	}
	// Reject the obviously-broken case rather than silently letting
	// it through. Slot or interval without URL would never be acted
	// on by the follower (`IsEnabled() == false`); flag it.
	if dep.Patroni.URL == "" && (dep.Patroni.Slot != "" || dep.Patroni.Interval != "" || len(dep.Patroni.Slots) > 0) {
		return output.NewError("usage.patroni_url_required",
			"deployment edit: --patroni-slot / --patroni-interval require a non-empty patroni-url").
			WithSuggestion(&output.Suggestion{
				Human: "pass --patroni-url alongside the slot/interval, or clear all three with --patroni-url \"\"",
			}).Wrap(output.ErrUsage)
	}

	// Re-probe if connection changed (and probe wasn't skipped).
	if opts.connectionSet && !opts.skipProbe && dep.PGConnection != "" {
		if err := probePG(cmd.Context(), dep.PGConnection); err != nil {
			return output.NewError("deployment.probe_failed",
				fmt.Sprintf("deployment edit: cannot reach PG: %v", err)).Wrap(err)
		}
	}

	cfg.Deployments[name] = dep
	if err := write(cfg); err != nil {
		return err
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(deploymentMutationBody{
		Name: name, Action: "edited",
	}))
}

func newDeploymentTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "test <name>",
		Short:        "Probe PG for a configured deployment",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeploymentTest(cmd, args[0])
		},
	}
}

func runDeploymentTest(cmd *cobra.Command, name string) error {
	d := DispatcherFrom(cmd)
	_, cfg, _, err := loadEditableConfig()
	if err != nil {
		return err
	}
	dep, err := mustHaveDeployment(cfg, name)
	if err != nil {
		return err
	}
	if dep.PGConnection == "" {
		return output.NewError("deployment.no_connection",
			fmt.Sprintf("deployment test: %q has no pg_connection set", name))
	}
	identity, version, err := probePGFull(cmd.Context(), dep.PGConnection)
	if err != nil {
		return output.NewError("deployment.probe_failed",
			fmt.Sprintf("deployment test: %v", err)).Wrap(err)
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(deploymentTestBody{
		Name:      name,
		PGVersion: version,
		SystemID:  identity.SystemID,
		Timeline:  uint32(identity.Timeline),
		Healthy:   true,
	}))
}

// probePG opens a replication-mode connection and runs IDENTIFY_SYSTEM.
// The smallest meaningful health check.
func probePG(ctx context.Context, dsn string) error {
	c, err := pg.Connect(ctx, dsn, pg.ModeReplication)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer c.Close(ctx)
	if _, err := pg.IdentifySystem(ctx, c); err != nil {
		return fmt.Errorf("IDENTIFY_SYSTEM: %w", err)
	}
	return nil
}

// probePGFull is probePG plus a regular-mode version query. Used by
// `deployment test` for the richer health line.
func probePGFull(ctx context.Context, dsn string) (pg.SystemIdentity, int, error) {
	c, err := pg.Connect(ctx, dsn, pg.ModeReplication)
	if err != nil {
		return pg.SystemIdentity{}, 0, fmt.Errorf("connect (replication): %w", err)
	}
	identity, err := pg.IdentifySystem(ctx, c)
	_ = c.Close(ctx)
	if err != nil {
		return pg.SystemIdentity{}, 0, fmt.Errorf("IDENTIFY_SYSTEM: %w", err)
	}

	rc, err := pg.Connect(ctx, dsn, pg.ModeRegular)
	if err != nil {
		// Replication probe succeeded — surface it; version is best-effort.
		return identity, 0, nil
	}
	defer rc.Close(ctx)
	v, err := pg.QueryVersion(ctx, rc)
	if err != nil {
		return identity, 0, nil
	}
	return identity, v.Major, nil
}

// Result body shapes — stable per the v1 schema commitment.

type deploymentListEntry struct {
	Name           string                 `json:"name"`
	PGConnection   string                 `json:"pg_connection,omitempty"`
	Repo           string                 `json:"repo,omitempty"`
	Tenant         string                 `json:"tenant,omitempty"`
	Classification string                 `json:"classification,omitempty"`
	BackupSchedule string                 `json:"backup_schedule"`
	RotateSchedule string                 `json:"rotate_schedule"`
	Patroni        *deploymentListPatroni `json:"patroni,omitempty"`
}

// deploymentListPatroni is the list-side view of PatroniConfig. Auth
// secrets (password, password_file) are never rendered — they're config
// inputs only.
type deploymentListPatroni struct {
	URL      string                      `json:"url,omitempty"`
	User     string                      `json:"user,omitempty"`
	Slot     string                      `json:"slot,omitempty"`
	Slots    []deploymentListPatroniSlot `json:"slots,omitempty"`
	Interval string                      `json:"interval,omitempty"`
}

type deploymentListPatroniSlot struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

type deploymentListBody struct {
	Deployments []deploymentListEntry `json:"deployments"`
}

// WriteText renders the deployment catalogue — including Patroni wiring when
// configured — as human-readable text to w.
func (b deploymentListBody) WriteText(w io.Writer) error {
	if len(b.Deployments) == 0 {
		_, err := fmt.Fprintln(w, "no deployments configured")
		return err
	}
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "%d deployment(s) configured\n", len(b.Deployments))
	for _, d := range b.Deployments {
		fmt.Fprintf(bw, "  %s\n", d.Name)
		if d.PGConnection != "" {
			fmt.Fprintf(bw, "    pg:        %s\n", d.PGConnection)
		}
		if d.Repo != "" {
			fmt.Fprintf(bw, "    repo:      %s\n", d.Repo)
		}
		if d.Tenant != "" {
			fmt.Fprintf(bw, "    tenant:    %s\n", d.Tenant)
		}
		if d.Classification != "" {
			fmt.Fprintf(bw, "    class:     %s\n", d.Classification)
		}
		fmt.Fprintf(bw, "    backup:    %s\n", d.BackupSchedule)
		fmt.Fprintf(bw, "    rotate:    %s\n", d.RotateSchedule)
		if d.Patroni != nil {
			if d.Patroni.URL != "" {
				fmt.Fprintf(bw, "    patroni-url: %s\n", d.Patroni.URL)
			}
			if d.Patroni.User != "" {
				fmt.Fprintf(bw, "    patroni-user: %s\n", d.Patroni.User)
			}
			if d.Patroni.Slot != "" {
				fmt.Fprintf(bw, "    slot:        %s\n", d.Patroni.Slot)
			}
			for _, s := range d.Patroni.Slots {
				fmt.Fprintf(bw, "    slot:        %s (%s)\n", s.Name, s.Role)
			}
			if d.Patroni.Interval != "" {
				fmt.Fprintf(bw, "    interval:    %s\n", d.Patroni.Interval)
			}
		}
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

type deploymentMutationBody struct {
	Name   string `json:"name"`
	Action string `json:"action"` // added | removed | edited
}

// WriteText renders the catalogue mutation as a single-line confirmation to w.
func (b deploymentMutationBody) WriteText(w io.Writer) error {
	_, err := fmt.Fprintf(w, "✓ Deployment %q %s", b.Name, b.Action)
	return err
}

type deploymentTestBody struct {
	Name      string `json:"name"`
	PGVersion int    `json:"pg_version,omitempty"`
	SystemID  string `json:"system_id"`
	Timeline  uint32 `json:"timeline"`
	Healthy   bool   `json:"healthy"`
}

// WriteText renders the deployment connectivity probe result as human-readable
// text to w.
func (b deploymentTestBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ %s reachable\n", b.Name)
	if b.PGVersion > 0 {
		fmt.Fprintf(bw, "  PostgreSQL: %d\n", b.PGVersion)
	}
	fmt.Fprintf(bw, "  Cluster ID: %s\n", b.SystemID)
	fmt.Fprintf(bw, "  Timeline:   %d", b.Timeline)
	_, err := io.WriteString(w, bw.String())
	return err
}
