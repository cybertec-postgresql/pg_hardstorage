// runbook.go — CLI surface for the disaster-runbook scenario catalogue.
package cli

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// newRunbookCmd implements `pg_hardstorage runbook` — the 3am-operator
// persona's "what do I do now?" surface.
//
// Two subcommands:
//
//	runbook list                                — enumerate scenarios
//	runbook generate <deployment> --scenario X  — emit Markdown to stdout
//
// Scenarios are versioned templates baked into the binary (no external
// files; the runbook content has to survive an air-gapped restore
// where the docs aren't reachable). Each renders a < 1 page Markdown
// document with the deployment's actual repo URL, keyring path, and
// connection string already substituted in — so the operator can
// copy-paste, not adapt.
//
// What's deliberately out of scope for v0.1:
//
//   - Free-form scenarios (the LLM helper's job).
//   - Markdown→PDF rendering (pipe through `pandoc` if needed).
//   - Per-customer runbook overrides (operators wanting custom
//     scenarios can write their own template + commit to ops repo).
func newRunbookCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "runbook",
		Short: "Generate a tailored runbook for the 3am operator",
		Long: `Each runbook is a step-by-step Markdown document with copy-pasteable
commands pre-filled with the deployment's actual configuration
(repo URL, deployment name, keyring path). They map to the
disaster scenarios documented in docs/SPEC.md (R1–R7).`,
	}
	c.AddCommand(newRunbookListCmd())
	c.AddCommand(newRunbookGenerateCmd())
	return c
}

func newRunbookListCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "list",
		Short:        "Enumerate available scenarios",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			return d.Result(output.NewResult(cmd.CommandPath()).
				WithBody(runbookListBody{Scenarios: scenarioCatalog()}))
		},
	}
}

func newRunbookGenerateCmd() *cobra.Command {
	var (
		scenario string
		repoURL  string
	)
	c := &cobra.Command{
		Use:   "generate <deployment>",
		Short: "Render a runbook for <deployment> + scenario as Markdown",
		Long: `Renders the chosen scenario's runbook to stdout as Markdown.
Scenario must be one of the names listed by ` + "`runbook list`" + `.

Behaviour with --output:

  - text (default) → bare Markdown, ready to pipe to pandoc / a wiki
  - json           → wrapped in the standard Result body so scripts can
                      extract just the .markdown field

The deployment must already exist in pg_hardstorage.yaml. --repo is
optional and overrides the deployment's configured Repo (useful when
generating a runbook for a recovery into a replica repo).`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRunbookGenerate(cmd, args[0], scenario, repoURL)
		},
	}
	c.Flags().StringVar(&scenario, "scenario", "",
		"scenario name (see `runbook list`) — required")
	_ = c.MarkFlagRequired("scenario")
	c.Flags().StringVar(&repoURL, "repo", "",
		"override the deployment's configured repo URL (optional)")
	return c
}

func runRunbookGenerate(cmd *cobra.Command, deployment, scenario, repoOverride string) error {
	d := DispatcherFrom(cmd)
	tmpl, ok := scenarioTemplates[scenario]
	if !ok {
		return output.NewError("usage.bad_scenario",
			fmt.Sprintf("runbook generate: unknown scenario %q", scenario)).
			WithSuggestion(&output.Suggestion{
				Human:   "see `pg_hardstorage runbook list` for valid names",
				Command: "pg_hardstorage runbook list",
			}).Wrap(output.ErrUsage)
	}

	// Load merged config; resolve the deployment.
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return output.NewError("internal", err.Error()).Wrap(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		return output.NewError("config.load_failed",
			fmt.Sprintf("runbook generate: %v", err)).Wrap(err)
	}
	// config.Load always returns a non-nil LoadResult on success,
	// so we can index Deployments unconditionally — a missing entry
	// (or a missing-deployments-map on a fresh install) yields the
	// notfound branch via the zero-value/false return from map index.
	dep, ok := cfg.Config.Deployments[deployment]
	if !ok {
		return output.NewError("notfound.deployment",
			fmt.Sprintf("runbook generate: no deployment %q in config",
				deployment)).
			WithSuggestion(&output.Suggestion{
				Human:   "list configured deployments with `pg_hardstorage deployment list`",
				Command: "pg_hardstorage deployment list",
			})
	}

	repoURL := dep.Repo
	if repoOverride != "" {
		repoURL = repoOverride
	}
	if repoURL == "" {
		return output.NewError("config.missing_repo",
			fmt.Sprintf("runbook generate: deployment %q has no repo configured", deployment)).
			WithSuggestion(&output.Suggestion{
				Human: "set repo via `pg_hardstorage deployment edit " + deployment + " --repo <url>` or pass --repo here",
			})
	}

	// Single timestamp — used for both the rendered runbook and the
	// surrounding Result body so consumers see one consistent value.
	now := time.Now().UTC().Format(time.RFC3339)
	// Empty tenant means "default" (per the SPEC's single-org user
	// model). Substitute here so templates don't have to spell out
	// the same "default-if-blank" branch every time.
	tenant := dep.Tenant
	if tenant == "" {
		tenant = "default"
	}
	rendered, err := renderRunbook(tmpl, runbookContext{
		Scenario:     scenario,
		Title:        scenarioTitles[scenario],
		Deployment:   deployment,
		RepoURL:      repoURL,
		PGConnection: redactDSN(dep.PGConnection),
		Tenant:       tenant,
		KeyringDir:   p.Keyring.Value,
		GeneratedAt:  now,
		BinaryName:   "pg_hardstorage",
	})
	if err != nil {
		return output.NewError("internal",
			fmt.Sprintf("runbook generate: render: %v", err)).Wrap(err)
	}

	body := runbookBody{
		Scenario:    scenario,
		Title:       scenarioTitles[scenario],
		Deployment:  deployment,
		Markdown:    rendered,
		GeneratedAt: now,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// runbookContext carries the per-deployment values templates can
// reference. Adding a new field here is a stable schema change for
// the runbook templates only — it's a private surface.
type runbookContext struct {
	Scenario     string
	Title        string
	Deployment   string
	RepoURL      string
	PGConnection string // redacted
	Tenant       string
	KeyringDir   string
	GeneratedAt  string
	BinaryName   string
}

func renderRunbook(tmpl string, ctx runbookContext) (string, error) {
	t, err := template.New("runbook").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	buf := &bytes.Buffer{}
	if err := t.Execute(buf, ctx); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}

// scenarioCatalog returns the sorted list of runbook scenarios with
// their human-readable titles. Used by `runbook list`.
func scenarioCatalog() []runbookScenario {
	out := make([]runbookScenario, 0, len(scenarioTitles))
	for name, title := range scenarioTitles {
		out = append(out, runbookScenario{
			Name:  name,
			Title: title,
			SPEC:  scenarioSPECRefs[name],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type runbookScenario struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	SPEC  string `json:"spec_ref,omitempty"` // e.g. "R1" → docs/SPEC.md disaster runbook
}

type runbookListBody struct {
	Scenarios []runbookScenario `json:"scenarios"`
}

// WriteText renders the available runbook scenarios as human-readable text to w.
func (b runbookListBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "%d scenario(s) available:\n", len(b.Scenarios))
	for _, s := range b.Scenarios {
		fmt.Fprintf(bw, "  %-12s  %s", s.Name, s.Title)
		if s.SPEC != "" {
			fmt.Fprintf(bw, "  [%s]", s.SPEC)
		}
		fmt.Fprintln(bw)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type runbookBody struct {
	Scenario    string `json:"scenario"`
	Title       string `json:"title"`
	Deployment  string `json:"deployment"`
	Markdown    string `json:"markdown"`
	GeneratedAt string `json:"generated_at"`
}

// WriteText emits the bare Markdown for the text renderer. JSON
// consumers get the same content under .result.body.markdown.
func (b runbookBody) WriteText(w io.Writer) error {
	_, err := io.WriteString(w, strings.TrimRight(b.Markdown, "\n"))
	return err
}
