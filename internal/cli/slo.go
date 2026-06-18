// slo.go — CLI surface for managing and reporting per-deployment RPO/RTO targets.
package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// newSloCmd implements `pg_hardstorage slo` — RPO/RTO targets per
// deployment, plus `slo report` which compares actual last-backup-
// age against the RPO target and flags breaches.
//
// Subcommands:
//
//	slo set <deployment> [--rpo 1h] [--rto 10m]
//	slo clear <deployment>
//	slo show [<deployment>]
//	slo report [<deployment>] [--repo <url>]
//
// `slo report` requires --repo (or the deployment to have a Repo
// configured) so we can read the latest manifest's StoppedAt and
// compute actual RPO. RTO is informational today — the verifier
// subsystem will correlate sandbox-restore timings with the
// declared target and flag misses.
//
// SLOs are advisory: a missed RPO surfaces as a finding in the
// report, not as an exit-code failure (so `slo report` in CI
// doesn't fail the pipeline on a transient backup delay). Operators
// wanting hard gates wire the report's JSON output into their
// monitoring tool's alert ladder.
func newSloCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "slo",
		Short: "RPO / RTO SLOs as code",
		Long: `Declare per-deployment recovery-point and recovery-time
objectives. ` + "`slo report`" + ` walks each deployment, reads the
latest manifest's StoppedAt, computes the actual RPO (now -
StoppedAt), and reports met / missed against the target.

Durations parse as Go-style ("1h", "30m", "24h", "7d"-shorthand
also accepted).`,
	}
	c.AddCommand(newSloSetCmd())
	c.AddCommand(newSloClearCmd())
	c.AddCommand(newSloShowCmd())
	c.AddCommand(newSloReportCmd())
	return c
}

func newSloSetCmd() *cobra.Command {
	var (
		rpoStr string
		rtoStr string
	)
	c := &cobra.Command{
		Use:          "set <deployment>",
		Short:        "Declare RPO / RTO targets for a deployment",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSloSet(cmd, args[0], rpoStr, rtoStr)
		},
	}
	c.Flags().StringVar(&rpoStr, "rpo", "",
		"max acceptable backup lag (e.g. 1h, 30m, 24h, 7d)")
	c.Flags().StringVar(&rtoStr, "rto", "",
		"max acceptable restore time (e.g. 10m, 1h)")
	return c
}

func runSloSet(cmd *cobra.Command, deployment, rpoStr, rtoStr string) error {
	d := DispatcherFrom(cmd)
	rpo, err := parseSLODuration(rpoStr)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("slo set: --rpo: %v", err)).Wrap(output.ErrUsage)
	}
	rto, err := parseSLODuration(rtoStr)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("slo set: --rto: %v", err)).Wrap(output.ErrUsage)
	}
	if rpoStr == "" && rtoStr == "" {
		return output.NewError("usage.missing_flag",
			"slo set: at least one of --rpo / --rto must be set (use `slo clear` to remove targets)").
			Wrap(output.ErrUsage)
	}

	_, cfg, write, err := loadEditableConfig()
	if err != nil {
		return err
	}
	dep, err := mustHaveDeployment(cfg, deployment)
	if err != nil {
		return err
	}
	if rpoStr != "" {
		dep.SLO.RPOSeconds = int64(rpo / time.Second)
	}
	if rtoStr != "" {
		dep.SLO.RTOSeconds = int64(rto / time.Second)
	}
	cfg.Deployments[deployment] = dep
	if err := write(cfg); err != nil {
		return err
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(sloMutationBody{
		Deployment: deployment,
		RPOSeconds: dep.SLO.RPOSeconds,
		RTOSeconds: dep.SLO.RTOSeconds,
	}))
}

func newSloClearCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "clear <deployment>",
		Short:        "Remove all SLO targets from a deployment",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := DispatcherFrom(cmd)
			_, cfg, write, err := loadEditableConfig()
			if err != nil {
				return err
			}
			dep, err := mustHaveDeployment(cfg, args[0])
			if err != nil {
				return err
			}
			dep.SLO = config.SLOConfig{}
			cfg.Deployments[args[0]] = dep
			if err := write(cfg); err != nil {
				return err
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(sloMutationBody{
				Deployment: args[0],
			}))
		},
	}
}

func newSloShowCmd() *cobra.Command {
	c := &cobra.Command{
		Use:          "show [<deployment>]",
		Short:        "Display configured SLO targets",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			scope := ""
			if len(args) == 1 {
				scope = args[0]
			}
			return runSloShow(cmd, scope)
		},
	}
	return c
}

func runSloShow(cmd *cobra.Command, scope string) error {
	d := DispatcherFrom(cmd)
	_, cfg, _, err := loadEditableConfig()
	if err != nil {
		return err
	}
	rows := []sloShowRow{}
	for name, dep := range cfg.Deployments {
		if scope != "" && name != scope {
			continue
		}
		rows = append(rows, sloShowRow{
			Deployment: name,
			RPOSeconds: dep.SLO.RPOSeconds,
			RTOSeconds: dep.SLO.RTOSeconds,
		})
	}
	if scope != "" && len(rows) == 0 {
		return output.NewError("notfound.deployment",
			fmt.Sprintf("slo show: deployment %q not in config", scope))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Deployment < rows[j].Deployment })
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(sloShowBody{
		Count:       len(rows),
		Deployments: rows,
	}))
}

func newSloReportCmd() *cobra.Command {
	var repoOverride string
	c := &cobra.Command{
		Use:          "report [<deployment>]",
		Short:        "Compare actual RPO against target; flag misses",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			scope := ""
			if len(args) == 1 {
				scope = args[0]
			}
			return runSloReport(cmd, scope, repoOverride)
		},
	}
	c.Flags().StringVar(&repoOverride, "repo", "",
		"override the deployment's configured repo (optional)")
	return c
}

func runSloReport(cmd *cobra.Command, scope, repoOverride string) error {
	d := DispatcherFrom(cmd)
	_, cfg, _, err := loadEditableConfig()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	rows := []sloReportRow{}
	for name, dep := range cfg.Deployments {
		if scope != "" && name != scope {
			continue
		}
		row := sloReportRow{
			Deployment: name,
			RPOTarget:  dep.SLO.RPOSeconds,
			RTOTarget:  dep.SLO.RTOSeconds,
		}
		repoURL := dep.Repo
		if repoOverride != "" {
			repoURL = repoOverride
		}
		if repoURL == "" {
			row.Status = "no_repo"
			row.Note = "deployment has no repo configured; cannot compute actual RPO"
			rows = append(rows, row)
			continue
		}
		row = computeRPOActual(cmd.Context(), row, repoURL, now)
		rows = append(rows, row)
	}
	if scope != "" && len(rows) == 0 {
		return output.NewError("notfound.deployment",
			fmt.Sprintf("slo report: deployment %q not in config", scope))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Deployment < rows[j].Deployment })
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(sloReportBody{
		Count:       len(rows),
		EvaluatedAt: now.Format(time.RFC3339),
		Deployments: rows,
	}))
}

// computeRPOActual reads the latest manifest's StoppedAt and fills
// in the row's RPOActual + Status. Errors during repo open / list
// are reported in Note rather than failing the whole report — a
// fleet-wide `slo report` should surface as much truth as it can.
func computeRPOActual(ctx context.Context, row sloReportRow, repoURL string, now time.Time) sloReportRow {
	verifier, err := loadVerifier()
	if err != nil {
		row.Status = "error"
		row.Note = "load verifier: " + err.Error()
		return row
	}
	_, sp, err := openRepo(ctx, repoURL)
	if err != nil {
		row.Status = "error"
		row.Note = "open repo: " + err.Error()
		return row
	}
	defer sp.Close()

	store := backup.NewManifestStore(sp)
	var latest time.Time
	for m, err := range store.List(ctx, row.Deployment, verifier) {
		if err != nil {
			continue
		}
		if m.StoppedAt.After(latest) {
			latest = m.StoppedAt
		}
	}
	if latest.IsZero() {
		row.Status = "no_backups"
		row.Note = "no backups committed for this deployment"
		return row
	}
	row.LatestBackup = latest.UTC().Format(time.RFC3339)
	actual := int64(now.Sub(latest) / time.Second)
	row.RPOActual = actual
	if row.RPOTarget == 0 {
		row.Status = "no_target"
		return row
	}
	if actual <= row.RPOTarget {
		row.Status = "met"
	} else {
		row.Status = "missed"
		row.Note = fmt.Sprintf("RPO target %s; actual %s (%s over)",
			fmtSeconds(row.RPOTarget),
			fmtSeconds(actual),
			fmtSeconds(actual-row.RPOTarget))
	}
	return row
}

// parseSLODuration is a permissive duration parser. Accepts Go's
// time.ParseDuration shapes ("1h", "30m") plus "<N>d" for
// day-shorthand which time.ParseDuration doesn't natively support
// but operators commonly use.
func parseSLODuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	// Day shorthand: "7d" → 7*24h. Stripped before time.ParseDuration.
	if strings.HasSuffix(s, "d") {
		nStr := strings.TrimSuffix(s, "d")
		var n int
		if _, err := fmt.Sscanf(nStr, "%d", &n); err != nil {
			return 0, fmt.Errorf("parse %q: bad day count", s)
		}
		if n < 0 {
			return 0, fmt.Errorf("parse %q: negative duration", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("parse %q: negative duration", s)
	}
	return d, nil
}

// fmtSeconds renders an integer-seconds duration as a compact
// human-readable string (e.g. "2h" / "47m" / "3d").
func fmtSeconds(secs int64) string {
	if secs == 0 {
		return "0s"
	}
	d := time.Duration(secs) * time.Second
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd%s", int(d/(24*time.Hour)), (d % (24 * time.Hour)).Truncate(time.Minute))
	}
	return d.Truncate(time.Second).String()
}

// Result body shapes — stable per the v1 schema commitment.

type sloMutationBody struct {
	Deployment string `json:"deployment"`
	RPOSeconds int64  `json:"rpo_seconds,omitempty"`
	RTOSeconds int64  `json:"rto_seconds,omitempty"`
}

// WriteText renders the SLO-set or SLO-clear confirmation as a single-line
// summary to w.
func (b sloMutationBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if b.RPOSeconds == 0 && b.RTOSeconds == 0 {
		fmt.Fprintf(bw, "✓ %s: SLO targets cleared", b.Deployment)
	} else {
		fmt.Fprintf(bw, "✓ %s: rpo=%s rto=%s",
			b.Deployment, fmtSeconds(b.RPOSeconds), fmtSeconds(b.RTOSeconds))
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

type sloShowRow struct {
	Deployment string `json:"deployment"`
	RPOSeconds int64  `json:"rpo_seconds,omitempty"`
	RTOSeconds int64  `json:"rto_seconds,omitempty"`
}

type sloShowBody struct {
	Count       int          `json:"count"`
	Deployments []sloShowRow `json:"deployments"`
}

// WriteText renders the per-deployment SLO targets as a tabular summary to w.
func (b sloShowBody) WriteText(w io.Writer) error {
	if len(b.Deployments) == 0 {
		_, err := fmt.Fprintln(w, "no deployments configured")
		return err
	}
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "%d deployment(s)\n", b.Count)
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  DEPLOYMENT\tRPO\tRTO")
	for _, r := range b.Deployments {
		rpo := defaultIfEmpty(fmtSecondsOrEmpty(r.RPOSeconds), "—")
		rto := defaultIfEmpty(fmtSecondsOrEmpty(r.RTOSeconds), "—")
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.Deployment, rpo, rto)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

func fmtSecondsOrEmpty(secs int64) string {
	if secs == 0 {
		return ""
	}
	return fmtSeconds(secs)
}

type sloReportRow struct {
	Deployment   string `json:"deployment"`
	RPOTarget    int64  `json:"rpo_target_seconds,omitempty"`
	RPOActual    int64  `json:"rpo_actual_seconds,omitempty"`
	RTOTarget    int64  `json:"rto_target_seconds,omitempty"`
	LatestBackup string `json:"latest_backup,omitempty"`
	// Status is one of: met / missed / no_target / no_backups /
	// no_repo / error.
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

type sloReportBody struct {
	Count       int            `json:"count"`
	EvaluatedAt string         `json:"evaluated_at"`
	Deployments []sloReportRow `json:"deployments"`
}

// WriteText renders the SLO target-vs-actual evaluation as a tabular summary to w.
func (b sloReportBody) WriteText(w io.Writer) error {
	if len(b.Deployments) == 0 {
		_, err := fmt.Fprintln(w, "no deployments configured")
		return err
	}
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "slo report — evaluated %s\n", b.EvaluatedAt)
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  DEPLOYMENT\tSTATUS\tRPO TARGET\tRPO ACTUAL\tNOTE")
	for _, r := range b.Deployments {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
			r.Deployment,
			statusGlyph(r.Status),
			defaultIfEmpty(fmtSecondsOrEmpty(r.RPOTarget), "—"),
			defaultIfEmpty(fmtSecondsOrEmpty(r.RPOActual), "—"),
			defaultIfEmpty(r.Note, "—"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

func statusGlyph(s string) string {
	switch s {
	case "met":
		return "✓ met"
	case "missed":
		return "✗ missed"
	default:
		return s
	}
}
