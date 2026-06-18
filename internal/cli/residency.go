// residency.go — CLI surface for setting and verifying per-deployment data residency.
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
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// newResidencyCmd implements `pg_hardstorage residency` — data
// residency pinning, the compliance primitive for "this deployment's
// backups MUST stay in {EU, US, ...}". Companion to `hold` (pin
// individual backups against retention) and `classify` (tag the
// data's sensitivity).
//
// Subcommands:
//
//	residency set <deployment> <region> [<region> ...]
//	residency clear <deployment>
//	residency list
//	residency check <deployment>
//
// The residency policy is stored on the deployment config and matched
// against the storage plugin's reported region (via the optional
// storage.RegionAware interface). Match is case-insensitive prefix-
// hyphen-aware: "eu" matches "eu-west-1" / "eu-central-1" but NOT
// "us-east-1". Exact-match works too: "eu-west-1" matches only
// "eu-west-1".
//
// Today this is an operator-run check. Automatic enforcement at
// backup-commit time is a lift (the runner needs a residency
// gate before pg_backup_start; ships the surface and `doctor`
// integration).
func newResidencyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "residency",
		Short: "Data-residency pinning for a deployment",
		Long: `Constrain a deployment's repository to a set of allowed regions.

Match rules: case-insensitive, hyphen-aware prefix.
  residency=["eu"]        matches eu-west-1, eu-central-1, ...
  residency=["eu-west-1"] matches only eu-west-1
  residency=[]            no constraint (default)

Storage plugins implement an optional Region() method; the fs
plugin reports the empty string ("region unknown") and FAILS any
non-empty residency check — local-disk repos can't enforce
residency, and silently treating that as a pass would defeat the
purpose.`,
	}
	c.AddCommand(
		newResidencySetCmd(),
		newResidencyClearCmd(),
		newResidencyListCmd(),
		newResidencyCheckCmd(),
	)
	return c
}

func newResidencySetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:          "set <deployment> <region> [<region> ...]",
		Short:        "Pin a deployment to one or more allowed regions",
		Args:         cobra.MinimumNArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runResidencySet(cmd, args[0], args[1:])
		},
	}
	return c
}

func runResidencySet(cmd *cobra.Command, deployment string, regions []string) error {
	d := DispatcherFrom(cmd)
	cleaned := make([]string, 0, len(regions))
	for _, r := range regions {
		r = strings.ToLower(strings.TrimSpace(r))
		if r == "" {
			return output.NewError("usage.bad_region",
				"residency set: empty region in argument list").Wrap(output.ErrUsage)
		}
		cleaned = append(cleaned, r)
	}
	_, cfg, write, err := loadEditableConfig()
	if err != nil {
		return err
	}
	dep, err := mustHaveDeployment(cfg, deployment)
	if err != nil {
		return err
	}
	previous := append([]string(nil), dep.Residency...)
	dep.Residency = cleaned
	cfg.Deployments[deployment] = dep
	if err := write(cfg); err != nil {
		return err
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(residencyMutationBody{
		Deployment: deployment,
		Previous:   previous,
		Current:    cleaned,
	}))
}

func newResidencyClearCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "clear <deployment>",
		Short:        "Remove residency constraint (allow any region)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runResidencySet(cmd, args[0], []string{})
		},
	}
}

func newResidencyListCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "list",
		Short:        "Show every deployment's residency policy",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runResidencyList(cmd)
		},
	}
}

func runResidencyList(cmd *cobra.Command) error {
	d := DispatcherFrom(cmd)
	_, cfg, _, err := loadEditableConfig()
	if err != nil {
		return err
	}
	rows := make([]residencyListRow, 0, len(cfg.Deployments))
	for name, dep := range cfg.Deployments {
		rows = append(rows, residencyListRow{
			Deployment: name,
			Regions:    append([]string(nil), dep.Residency...),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Deployment < rows[j].Deployment })
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(residencyListBody{
		Count:       len(rows),
		Deployments: rows,
	}))
}

func newResidencyCheckCmd() *cobra.Command {
	c := &cobra.Command{
		Use:          "check <deployment>",
		Short:        "Verify the configured repo's region matches the deployment's residency policy",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runResidencyCheck(cmd, args[0])
		},
	}
	return c
}

func runResidencyCheck(cmd *cobra.Command, deployment string) error {
	d := DispatcherFrom(cmd)
	_, cfg, _, err := loadEditableConfig()
	if err != nil {
		return err
	}
	dep, err := mustHaveDeployment(cfg, deployment)
	if err != nil {
		return err
	}
	if dep.Repo == "" {
		return output.NewError("config.missing_repo",
			fmt.Sprintf("residency check: deployment %q has no repo configured", deployment))
	}

	_, sp, err := openRepo(cmd.Context(), dep.Repo)
	if err != nil {
		return err
	}
	defer sp.Close()
	region := storage.RegionOf(sp)

	body := residencyCheckBody{
		Deployment: deployment,
		Repo:       dep.Repo,
		Region:     region,
		Allowed:    append([]string(nil), dep.Residency...),
	}
	body.Compliant, body.Reason = checkResidency(region, dep.Residency)

	if !body.Compliant {
		// verify.* namespace routes to ExitVerifyFailed (9), same as
		// `repo check`'s missing-chunks finding — a residency
		// violation is, semantically, "your backup-target setup
		// doesn't meet the declared policy."
		return output.NewError("verify.residency_violation",
			fmt.Sprintf("residency check: %s", body.Reason)).
			WithSuggestion(&output.Suggestion{
				Human:   "either update the deployment's repo to a region that matches the policy, or relax the policy with `pg_hardstorage residency set` / `clear`.",
				Command: "pg_hardstorage residency list",
			})
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// checkResidency is the matcher. Returns (compliant, reason).
//
//   - allowed empty                 → compliant (no constraint)
//   - region empty (RegionUnknown)  → not compliant when allowed
//     non-empty (local-disk repo
//     can't enforce residency)
//   - allowed contains the region   → compliant
//   - allowed contains a prefix     → compliant if region starts
//     with prefix + "-"
//
// Match is case-insensitive throughout.
func checkResidency(region string, allowed []string) (bool, string) {
	if len(allowed) == 0 {
		return true, "no residency constraint"
	}
	if region == storage.RegionUnknown {
		return false, fmt.Sprintf("repo region is unknown (likely a local fs repo); residency policy %v cannot be enforced",
			allowed)
	}
	r := strings.ToLower(region)
	for _, a := range allowed {
		a = strings.ToLower(a)
		if a == r {
			return true, fmt.Sprintf("region %q matches policy entry %q exactly", region, a)
		}
		if strings.HasPrefix(r, a+"-") {
			return true, fmt.Sprintf("region %q matches policy prefix %q", region, a)
		}
	}
	return false, fmt.Sprintf("region %q does not match any allowed entry %v",
		region, allowed)
}

// CheckDeploymentResidency is the public-package version of
// runResidencyCheck without CLI plumbing — used by future
// enforcement gates (backup orchestrator's pre-flight) and by
// doctor for fleet-wide reporting.
func CheckDeploymentResidency(sp storage.StoragePlugin, dep config.DeploymentConfig) (bool, string) {
	return checkResidency(storage.RegionOf(sp), dep.Residency)
}

// Result body shapes — stable per the v1 schema commitment.

type residencyMutationBody struct {
	Deployment string   `json:"deployment"`
	Previous   []string `json:"previous"`
	Current    []string `json:"current"`
}

// WriteText renders the residency-set result — previous and current allowed
// regions — as a single-line confirmation to w.
func (b residencyMutationBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if len(b.Current) == 0 {
		fmt.Fprintf(bw, "✓ %s: residency cleared (was %v)", b.Deployment, b.Previous)
	} else {
		fmt.Fprintf(bw, "✓ %s: residency = %v (was %v)",
			b.Deployment, b.Current, b.Previous)
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

type residencyListRow struct {
	Deployment string   `json:"deployment"`
	Regions    []string `json:"regions"`
}

type residencyListBody struct {
	Count       int                `json:"count"`
	Deployments []residencyListRow `json:"deployments"`
}

// WriteText renders the per-deployment residency list as a tabular summary to w.
func (b residencyListBody) WriteText(w io.Writer) error {
	if len(b.Deployments) == 0 {
		_, err := fmt.Fprintln(w, "no deployments configured")
		return err
	}
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "%d deployment(s)\n", b.Count)
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  DEPLOYMENT\tRESIDENCY")
	for _, r := range b.Deployments {
		val := "—"
		if len(r.Regions) > 0 {
			val = strings.Join(r.Regions, ", ")
		}
		fmt.Fprintf(tw, "  %s\t%s\n", r.Deployment, val)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type residencyCheckBody struct {
	Deployment string   `json:"deployment"`
	Repo       string   `json:"repo"`
	Region     string   `json:"region"`
	Allowed    []string `json:"allowed"`
	Compliant  bool     `json:"compliant"`
	Reason     string   `json:"reason"`
}

// WriteText renders the residency-check verdict — region against allowlist —
// as human-readable text to w.
func (b residencyCheckBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "residency check — %s\n", b.Deployment)
	fmt.Fprintf(bw, "  Repo:      %s\n", b.Repo)
	if b.Region == "" {
		fmt.Fprintf(bw, "  Region:    (unknown / local fs)\n")
	} else {
		fmt.Fprintf(bw, "  Region:    %s\n", b.Region)
	}
	if len(b.Allowed) == 0 {
		fmt.Fprintf(bw, "  Allowed:   (no constraint)\n")
	} else {
		fmt.Fprintf(bw, "  Allowed:   %s\n", strings.Join(b.Allowed, ", "))
	}
	if b.Compliant {
		fmt.Fprintf(bw, "  ✓ %s", b.Reason)
	} else {
		fmt.Fprintf(bw, "  ✗ %s", b.Reason)
	}
	_, err := io.WriteString(w, bw.String())
	return err
}
