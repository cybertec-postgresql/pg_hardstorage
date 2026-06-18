// pg_hardstorage_testkit — first-class test infrastructure for pg_hardstorage.
//
// v0.1 ships:
//
//	scenario lint <file>           # validate the YAML against the v1 schema
//	scenario run <file>            # bring up topology, drive load, run asserts
//	load lint <file>               # validate load YAML
//	load checkpoint show <file>    # introspect checkpoint NDJSON
//	topology list                  # list registered providers
//	version
//
// Heavier subcommands (matrix expand/run/report, inject network/disk,
// differential vs pgBackRest/WAL-G, coverage report, bisect) ship in
// as the verifier subsystem matures.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/bisect"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/coverage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/load"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/scenario"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/topology"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/version"
)

func main() {
	root := &cobra.Command{
		Use:           "pg_hardstorage_testkit",
		Short:         "Test infrastructure for pg_hardstorage.",
		Long:          "Topology providers, deterministic load engine, assertion DSL, scenario runner.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		newVersionCmd(),
		newScenarioCmd(),
		newLoadCmd(),
		newTopologyCmd(),
		newFleetCmd(),
		newProfileCmd(),
		newFaultCmd(),
		newImageCmd(),
		newComposeCmd(),
		newValidateCmd(),
		newWatchCmd(),
		newK8sCmd(),
		newCoverageCmd(),
		newBisectCmd(),
		newLLMCmd(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// --- version ----------------------------------------------------------

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "Print version information",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "pg_hardstorage_testkit %s (%s, built %s)\n",
				version.Version, version.Commit, version.Date)
			return nil
		},
	}
}

// --- scenario ---------------------------------------------------------

func newScenarioCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "scenario <lint|run>",
		Short: "Manage and run test scenarios",
	}
	c.AddCommand(newScenarioLintCmd(), newScenarioRunCmd())
	return c
}

func newScenarioLintCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lint <file>",
		Short: "Validate a scenario YAML against the v1 schema",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := scenario.FromFile(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"✓ scenario %q is valid (tier=%s, provider=%s, %d steps, %d asserts)\n",
				s.Name, s.Tier, s.Topology.Provider, len(s.Steps), len(s.Asserts))
			return nil
		},
	}
}

func newScenarioRunCmd() *cobra.Command {
	var (
		artefactDir string
		skipTopo    bool
		airgap      bool
	)
	c := &cobra.Command{
		Use:   "run <file>",
		Short: "Execute a scenario end-to-end",
		Long: `Bring up the topology, drive the load file's operations,
run each step in sequence, evaluate every assertion, and tear down
on success (or keep the artefacts on failure for forensics).

The run emits NDJSON progress events to stdout so a test harness can
pipe through jq. The structured Result lands at <artefact-dir>/result.json
when the run finishes (success or failure).

Use --skip-topology to point at an already-running PG via the
PG_HARDSTORAGE_TESTKIT_DSN environment variable; useful when
debugging a scenario interactively.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := scenario.FromFile(args[0])
			if err != nil {
				return err
			}
			res, err := runner.Run(cmd.Context(), s, runner.RunOptions{
				Out:          cmd.OutOrStdout(),
				ArtefactDir:  artefactDir,
				SkipTopology: skipTopo,
				Airgap:       airgap,
			})
			if err != nil {
				return err
			}
			if !res.Pass {
				return fmt.Errorf("scenario failed: %s", res.Failure)
			}
			return nil
		},
	}
	c.Flags().StringVar(&artefactDir, "artefact-dir", "",
		"directory for run artefacts (default: temp dir)")
	c.Flags().BoolVar(&skipTopo, "skip-topology", false,
		"skip Up/Down; use $PG_HARDSTORAGE_TESTKIT_DSN")
	c.Flags().BoolVar(&airgap, "airgap", false,
		"refuse to docker-pull missing sink images at runtime; pre-flight fails fast "+
			"with a hint to `image pull-sinks` instead")
	return c
}

// --- load -------------------------------------------------------------

func newLoadCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "load <lint|checkpoint>",
		Short: "Validate load YAML and inspect checkpoint NDJSON",
	}
	c.AddCommand(
		&cobra.Command{
			Use:   "lint <file>",
			Short: "Validate a load YAML",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				l, err := load.LoadFromFile(args[0])
				if err != nil {
					return err
				}
				ops := 0
				for _, p := range l.Phases {
					ops += len(p.Operations)
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"✓ load is valid (seed=%d, %d phases, %d ops total)\n",
					l.Seed, len(l.Phases), ops)
				return nil
			},
		},
		newLoadCheckpointCmd(),
	)
	return c
}

func newLoadCheckpointCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "checkpoint <show>",
		Short: "Inspect checkpoint NDJSON files",
	}
	c.AddCommand(&cobra.Command{
		Use:   "show <file>",
		Short: "Print every checkpoint in the file as pretty JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer f.Close()
			dec := json.NewDecoder(f)
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			for {
				var v any
				if err := dec.Decode(&v); err != nil {
					if err == io.EOF {
						return nil
					}
					return err
				}
				if err := enc.Encode(v); err != nil {
					return err
				}
			}
		},
	})
	return c
}

// --- topology ---------------------------------------------------------

func newTopologyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "topology <list>",
		Short: "Topology providers",
	}
	c.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List registered topology providers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			providers := []struct {
				name, status string
			}{
				{"local-docker", "ready — testcontainers-managed PG"},
				{"testcontainers", "ready — alias of local-docker"},
				{"kind", "planned"},
				{"k8s-remote", "planned"},
				{"ssh-inventory", "planned"},
				{"cloud-vms", "planned"},
				{"firecracker", "planned"},
			}
			for _, p := range providers {
				fmt.Fprintf(cmd.OutOrStdout(), "  %-18s %s\n", p.name, p.status)
			}
			// Touch the builder so a future contributor renaming a
			// provider gets a compile error rather than just a
			// list-display drift.
			_, _ = topology.Build("local-docker")
			_ = context.Background
			return nil
		},
	})
	return c
}

// --- coverage ---------------------------------------------------------

func newCoverageCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "coverage <report>",
		Short: "Coverage view across code paths, matrix cells, scenarios",
	}
	c.AddCommand(newCoverageReportCmd(), newCoverageCLICmd())
	return c
}

func newCoverageReportCmd() *cobra.Command {
	var profileGlob string
	c := &cobra.Command{
		Use:   "report",
		Short: "Aggregate harvested NDJSON profiles into a coverage report",
		Long: `Reads every NDJSON profile under --profiles and produces an
aggregated report sortable by file, scenario, or matrix cell.

The report's "lowest-coverage files" punch-list tells the
operator where to add tests.  Output goes to stdout as JSON
unless --text is set.

Profile NDJSON shape (one Profile per line):

  {"schema":"pg_hardstorage.testkit.coverage.v1",
   "scenario":"wal-failover-1",
   "matrix_cell":"ubuntu-22.04/pg-17/ext4",
   "harvested_at":"2026-04-28T09:12:00Z",
   "files":{"internal/wal/stream/follower.go":80.0}}

Profiles are written by the runner during scenario execution.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if profileGlob == "" {
				return fmt.Errorf("coverage report: --profiles <glob> is required")
			}
			matches, err := filepath_Glob(profileGlob)
			if err != nil {
				return err
			}
			if len(matches) == 0 {
				return fmt.Errorf("coverage report: no profiles match %q", profileGlob)
			}
			var all []coverage.Profile
			for _, p := range matches {
				f, err := os.Open(p)
				if err != nil {
					return err
				}
				profiles, err := coverage.LoadProfiles(f)
				f.Close()
				if err != nil {
					return fmt.Errorf("coverage report: %s: %w", p, err)
				}
				all = append(all, profiles...)
			}
			rep := coverage.Aggregate(all)
			return rep.WriteText(cmd.OutOrStdout())
		},
	}
	c.Flags().StringVar(&profileGlob, "profiles", "",
		"glob for NDJSON profile files (e.g. \"./testkit-out/*.ndjson\")")
	return c
}

// --- bisect -----------------------------------------------------------

func newBisectCmd() *cobra.Command {
	var (
		badSHA    string
		goodSHA   string
		runScript string
	)
	c := &cobra.Command{
		Use:   "bisect",
		Short: "Binary-search the regressing commit for a scenario",
		Long: `Walks the commit range from --bad (newest) to --good (oldest),
runs --run-script per candidate, and reports the first commit
whose run exits non-zero.

The --run-script is invoked with the candidate SHA as a single
argument; it should:

  1. git checkout the SHA
  2. rebuild the binary
  3. run the scenario harness
  4. exit 0 (Good), 1 (Bad), 125 (Skip — the git-bisect convention)

Same shape as ` + "`git bisect run`" + ` but with structured output
and Step-trace recording.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if badSHA == "" || goodSHA == "" {
				return fmt.Errorf("bisect: --bad and --good are required")
			}
			if runScript == "" {
				return fmt.Errorf("bisect: --run-script is required")
			}
			commits, err := gitLogRange(cmd.Context(), badSHA, goodSHA)
			if err != nil {
				return err
			}
			res, err := bisect.Run(cmd.Context(), bisect.Options{
				CommitRange: commits,
				Runner:      shellRunner(runScript),
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"first-bad: %s (steps=%d, skipped=%d)\n",
				res.FirstBadSHA, len(res.Steps), len(res.Skipped))
			return nil
		},
	}
	c.Flags().StringVar(&badSHA, "bad", "", "newest commit known to be Bad (the regressor or later)")
	c.Flags().StringVar(&goodSHA, "good", "", "oldest commit known to be Good (the pre-regressor)")
	c.Flags().StringVar(&runScript, "run-script", "",
		"path to a script invoked with the candidate SHA (must exit 0/1/125)")
	return c
}

// --- bisect / coverage helpers ----------------------------------------

// filepath_Glob wraps filepath.Glob so the call site reads
// uniformly with the rest of the testkit's filesystem helpers.
func filepath_Glob(pattern string) ([]string, error) {
	return filepath.Glob(pattern)
}

// shellRunner returns a bisect.Runner that invokes the supplied
// shell script with the candidate SHA as an argument and maps
// the exit code to the bisect.Outcome enum (0=Good, 1=Bad,
// 125=Skip — the git-bisect convention).  Other exit codes are
// surfaced as errors.
func shellRunner(script string) bisect.Runner {
	return func(ctx context.Context, sha string) (bisect.Outcome, error) {
		cmd := exec.CommandContext(ctx, script, sha)
		err := cmd.Run()
		if err == nil {
			return bisect.Good, nil
		}
		if ee, ok := err.(*exec.ExitError); ok {
			switch ee.ExitCode() {
			case 1:
				return bisect.Bad, nil
			case 125:
				return bisect.Skip, nil
			default:
				return bisect.Skip, fmt.Errorf("bisect: %s exited %d (treating as Skip)",
					script, ee.ExitCode())
			}
		}
		return bisect.Skip, fmt.Errorf("bisect: %s: %w", script, err)
	}
}

// gitLogRange returns SHAs from bad..good in newest-first
// order — the shape bisect.Run expects.
func gitLogRange(ctx context.Context, bad, good string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "log", "--pretty=%H",
		fmt.Sprintf("%s...%s", good, bad))
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log %s..%s: %w", good, bad, err)
	}
	var commits []string
	for _, line := range splitLines(out) {
		if line == "" {
			continue
		}
		commits = append(commits, line)
	}
	if len(commits) == 0 {
		return nil, fmt.Errorf("git log returned no commits between %s and %s", good, bad)
	}
	// Ensure both endpoints are explicitly present.  git log's
	// `good...bad` syntax excludes the older boundary.
	if commits[len(commits)-1] != good {
		commits = append(commits, good)
	}
	if commits[0] != bad {
		commits = append([]string{bad}, commits...)
	}
	return commits, nil
}

func splitLines(b []byte) []string {
	out := []string{}
	start := 0
	for i, ch := range b {
		if ch == '\n' {
			out = append(out, string(b[start:i]))
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}
