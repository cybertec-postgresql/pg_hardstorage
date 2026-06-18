// classify.go — CLI surface for setting and listing deployment data classifications.
package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Classification levels — the four-step ladder common to most
// enterprise data-handling policies (NIST 800-53 / ISO 27001 / SOC-2).
// Values are exposed as the `classification:` YAML key on each
// deployment and referenced by future enforcement (per-class
// retention floor, region pinning, required-encryption).
const (
	ClassPublic       = "public"
	ClassInternal     = "internal"
	ClassConfidential = "confidential"
	ClassRestricted   = "restricted"
)

// classOrder is the canonical ordering for sort + display. Lower
// indexes are LESS sensitive (public is at the bottom of the
// scale); the future enforcement layer ("at least X") will use
// this ordering.
var classOrder = map[string]int{
	ClassPublic:       0,
	ClassInternal:     1,
	ClassConfidential: 2,
	ClassRestricted:   3,
}

// classRank returns the sort rank for a classification, with a
// defensive bias: unknown values rank ABOVE all known levels so a
// hand-edited typo or a future-version tag we don't recognise sorts
// to the top of the auditor's "most sensitive" view, not the bottom
// alongside "public". The naive `classOrder[level]` returns Go's
// zero value (0 = ClassPublic) for unknown keys — exactly the wrong
// default for a compliance feature.
func classRank(level string) int {
	if r, ok := classOrder[level]; ok {
		return r
	}
	return len(classOrder) // strictly greater than every known rank
}

func validClassification(level string) bool {
	_, ok := classOrder[level]
	return ok
}

// newClassifyCmd implements the `classify` command tree.
//
//	classify set <deployment> <level>      set the classification
//	classify list                          show all deployments + tags
//
// Setting a level mutates the per-deployment `classification:` key in
// pg_hardstorage.yaml. A deployment without an explicit tag is
// reported with the implicit default ("internal") so JSON consumers
// always see a value; the result body's `explicit` field
// distinguishes operator-set from default.
//
// What's deliberately NOT here for v0.1's pull-forward:
//
//   - Enforcement (per-class retention floor, allowed-region check,
//     required-encryption gate) — the SPEC's+ work; today's
//     classify is a tag the operator + future enforcement read.
//   - Per-tenant default (a `tenant: prod, default-class: confidential`
//     config block); add when a multi-tenant SaaS user shows up.
func newClassifyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "classify",
		Short: "Apply a data-classification tag to a deployment",
		Long: `Tag deployments with a data-sensitivity level. Levels (least to
most sensitive): public, internal, confidential, restricted.

The tag is informational in v0.1 — surfaced in doctor and stored
in pg_hardstorage.yaml. wires it to retention floor, allowed
regions, and required-encryption enforcement.`,
	}
	c.AddCommand(newClassifySetCmd())
	c.AddCommand(newClassifyListCmd())
	return c
}

// `classify set <dep> <level>` is the only mutating shape. Show is
// covered by `classify list` (or `deployment list` / `doctor`); a
// dedicated `classify show` would be redundant given those surfaces.
func newClassifySetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "set <deployment> <level>",
		Short: "Tag a deployment with public / internal / confidential / restricted",
		Args:  cobra.ExactArgs(2),
		Long: `Sets the classification of <deployment> to <level>. Allowed
levels (in increasing sensitivity):

  public         no expectation of privacy
  internal       default — staff-readable
  confidential   PII / regulated data
  restricted     highest sensitivity (PCI / HIPAA / financial)`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClassifySet(cmd, args[0], args[1])
		},
	}
	return c
}

// runClassifySet mutates the per-deployment classification field.
func runClassifySet(cmd *cobra.Command, deployment, level string) error {
	d := DispatcherFrom(cmd)
	level = strings.ToLower(strings.TrimSpace(level))
	if !validClassification(level) {
		return output.NewError("usage.bad_classification",
			fmt.Sprintf("classify set: unknown level %q (allowed: %s)",
				level, strings.Join(canonicalClassList(), ", "))).
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
	previous := effectiveClassification(dep.Classification)
	dep.Classification = level
	cfg.Deployments[deployment] = dep
	if err := write(cfg); err != nil {
		return err
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(classifyMutationBody{
		Deployment: deployment,
		Previous:   previous,
		Current:    level,
	}))
}

func newClassifyListCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "list",
		Short:        "Show every deployment's classification",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClassifyList(cmd)
		},
	}
}

func runClassifyList(cmd *cobra.Command) error {
	d := DispatcherFrom(cmd)
	_, cfg, _, err := loadEditableConfig()
	if err != nil {
		return err
	}
	rows := make([]classifyListRow, 0, len(cfg.Deployments))
	for name, dep := range cfg.Deployments {
		eff := effectiveClassification(dep.Classification)
		rows = append(rows, classifyListRow{
			Deployment:     name,
			Classification: eff,
			Explicit:       dep.Classification != "",
			Valid:          validClassification(eff),
		})
	}
	// Sort by sensitivity-descending, then by name. Operators auditing
	// "what's restricted?" see those at the top — and unknown values
	// (typos, future-version tags) sort ABOVE restricted via classRank's
	// defensive bias, so they're impossible to miss.
	sort.Slice(rows, func(i, j int) bool {
		ri, rj := classRank(rows[i].Classification), classRank(rows[j].Classification)
		if ri != rj {
			return ri > rj
		}
		return rows[i].Deployment < rows[j].Deployment
	})
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(classifyListBody{
		Deployments: rows,
		Count:       len(rows),
	}))
}

// effectiveClassification returns the operator-visible classification
// for a deployment. Empty config field means "internal" — the
// implicit default. This keeps the JSON schema's classification
// field non-empty for every deployment so consumers don't have to
// special-case the absent value.
func effectiveClassification(raw string) string {
	if raw == "" {
		return ClassInternal
	}
	return raw
}

func canonicalClassList() []string {
	out := make([]string, 0, len(classOrder))
	for k := range classOrder {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		return classOrder[out[i]] < classOrder[out[j]]
	})
	return out
}

// Result body shapes — stable per the v1 schema commitment.

type classifyMutationBody struct {
	Deployment string `json:"deployment"`
	Previous   string `json:"previous"`
	Current    string `json:"current"`
}

// WriteText renders the classification-change result as a single-line
// confirmation to w.
func (b classifyMutationBody) WriteText(w io.Writer) error {
	if b.Previous == b.Current {
		_, err := fmt.Fprintf(w, "✓ %s: classification already %s", b.Deployment, b.Current)
		return err
	}
	_, err := fmt.Fprintf(w, "✓ %s: classification %s → %s",
		b.Deployment, b.Previous, b.Current)
	return err
}

type classifyListRow struct {
	Deployment     string `json:"deployment"`
	Classification string `json:"classification"`
	// Explicit reports whether the deployment has an operator-set
	// classification (true) or is using the implicit default (false).
	// Future enforcement gates can refuse "implicit defaults on
	// restricted-data deployments" — surfacing this distinction at
	// the API level keeps the operator's intent visible.
	Explicit bool `json:"explicit"`
	// Valid reports whether the classification value is one of the
	// canonical four (public/internal/confidential/restricted). Hand-
	// edited typos in YAML produce Valid=false; the operator's
	// monitoring tools can flag these without re-implementing the
	// canonical list.
	Valid bool `json:"valid"`
}

type classifyListBody struct {
	Count       int               `json:"count"`
	Deployments []classifyListRow `json:"deployments"`
}

// WriteText renders the per-deployment classification list as a tabular
// summary to w.
func (b classifyListBody) WriteText(w io.Writer) error {
	if len(b.Deployments) == 0 {
		_, err := fmt.Fprintln(w, "no deployments configured")
		return err
	}
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "%d deployment(s)\n", b.Count)
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  DEPLOYMENT\tCLASSIFICATION\tSOURCE")
	for _, r := range b.Deployments {
		src := "explicit"
		if !r.Explicit {
			src = "default"
		}
		if !r.Valid {
			// Make hand-edited typos visible to the operator scanning
			// the text view — the JSON consumer gets the same signal
			// via row.valid=false.
			src = "INVALID"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.Deployment, r.Classification, src)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
