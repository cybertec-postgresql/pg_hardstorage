// coverage_cli.go — `coverage cli` subcommand: gates which cobra leaves are exercised by scenarios.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// `coverage cli` — CLI-surface coverage gate.
//
// Walks the cobra tree (via `pg_hardstorage __dump-cmd-tree`) +
// every scenario YAML under test/scenarios/, and reports which
// leaf commands have ≥1 scenario covering them.  Exits non-zero
// when any uncovered leaf is not in the allow-list.
//
// Scenario coverage is detected three ways:
//   1. cli_run steps with `args: ["wal", "stream", ...]` — the
//      direct form.
//   2. Wrapper step kinds the runner maps to a CLI invocation
//      (take_backup → backup, restore → restore,
//      wal_stream → wal stream).
//   3. Allow-list entries in .testkit/cli_coverage_allowlist.txt
//      (one leaf path per line; "#" starts a comment).
//
// The allow-list is for leaves that legitimately can't be
// exercised in a scenario — interactive prompts, things that
// would shell out to a real LLM provider, etc.  Keep it short
// and reviewed.

// stepWrapperMap encodes the runner.go switch in
// internal/testkit/runner/runner.go — step kinds that shell out
// to a specific pg_hardstorage subcommand.  Keep in lockstep
// with that file.
var stepWrapperMap = map[string][]string{
	"take_backup": {"backup"},
	"restore":     {"restore"},
	"wal_stream":  {"wal stream"},
}

type cmdTreeNode struct {
	Path           string `json:"path"`
	Runnable       bool   `json:"runnable"`
	HasSubcommands bool   `json:"has_subcommands"`
	Hidden         bool   `json:"hidden"`
}

func newCoverageCLICmd() *cobra.Command {
	var (
		agentBin     string
		scenarioDir  string
		allowFile    string
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "cli",
		Short: "Gate: every cobra leaf must be covered by ≥1 scenario",
		Long: `Compare the cobra command tree against scenario YAML
to find leaves that have no scenario exercising them.

Exits non-zero when any uncovered leaf is not in the allow-list.
Wire into CI to prevent new commands landing without a scenario.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			leaves, err := loadCmdTree(agentBin)
			if err != nil {
				return fmt.Errorf("dump cmd tree: %w", err)
			}
			covered, err := scanScenarios(scenarioDir, leaves)
			if err != nil {
				return fmt.Errorf("scan scenarios: %w", err)
			}
			allow, err := loadAllowlist(allowFile)
			if err != nil {
				return fmt.Errorf("load allowlist: %w", err)
			}

			leafSet := make(map[string]bool, len(leaves))
			for _, l := range leaves {
				leafSet[l] = true
			}
			var allLeaves []string
			for l := range leafSet {
				allLeaves = append(allLeaves, l)
			}
			sort.Strings(allLeaves)

			var uncovered, allowed []string
			for _, l := range allLeaves {
				if covered[l] {
					continue
				}
				if allow[l] {
					allowed = append(allowed, l)
					continue
				}
				uncovered = append(uncovered, l)
			}

			result := struct {
				Total       int      `json:"total"`
				Covered     int      `json:"covered"`
				Uncovered   []string `json:"uncovered"`
				Allowlisted []string `json:"allowlisted"`
			}{
				Total:       len(allLeaves),
				Covered:     len(allLeaves) - len(uncovered) - len(allowed),
				Uncovered:   uncovered,
				Allowlisted: allowed,
			}

			switch outputFormat {
			case "json":
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(result); err != nil {
					return err
				}
			default:
				fmt.Printf("CLI coverage: %d/%d covered (%d allow-listed, %d uncovered)\n",
					result.Covered, result.Total, len(allowed), len(uncovered))
				if len(uncovered) > 0 {
					fmt.Println()
					fmt.Println("Uncovered leaves:")
					for _, l := range uncovered {
						fmt.Printf("  - %s\n", l)
					}
				}
			}

			if len(uncovered) > 0 {
				return fmt.Errorf("%d leaf command(s) have no scenario coverage", len(uncovered))
			}
			return nil
		},
	}
	c.Flags().StringVar(&agentBin, "agent-bin", "./bin/pg_hardstorage",
		"path to the pg_hardstorage binary (used to dump the cobra tree)")
	c.Flags().StringVar(&scenarioDir, "scenarios", "test/scenarios",
		"directory containing scenario YAMLs")
	c.Flags().StringVar(&allowFile, "allowlist", ".testkit/cli_coverage_allowlist.txt",
		"path to the allow-list file (one leaf per line; '#' starts a comment)")
	c.Flags().StringVar(&outputFormat, "output", "text",
		"output format: text | json")
	return c
}

// loadCmdTree runs `<agentBin> __dump-cmd-tree` and returns every
// runnable, non-hidden leaf path.  A "leaf" here means any node
// that itself is runnable — including parents that are both
// runnable and have subcommands (e.g. `backup`).
func loadCmdTree(agentBin string) ([]string, error) {
	if _, err := os.Stat(agentBin); err != nil {
		return nil, fmt.Errorf("agent binary not found at %q (build it with `make` or `./compile.sh`)", agentBin)
	}
	out, err := exec.Command(agentBin, "__dump-cmd-tree").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("__dump-cmd-tree: exit %d: %s", ee.ExitCode(), string(ee.Stderr))
		}
		return nil, err
	}
	var nodes []cmdTreeNode
	if err := json.Unmarshal(out, &nodes); err != nil {
		return nil, fmt.Errorf("parse cmd-tree JSON: %w", err)
	}
	var leaves []string
	for _, n := range nodes {
		if !n.Runnable || n.Hidden {
			continue
		}
		// Skip pure-passthrough subcommands the user shouldn't need
		// to write scenarios for.
		base := lastToken(n.Path)
		if base == "help" || base == "completion" {
			continue
		}
		leaves = append(leaves, n.Path)
	}
	return leaves, nil
}

func lastToken(p string) string {
	parts := strings.Fields(p)
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// scanScenarios walks scenarioDir, parses every YAML, and returns
// the set of leaves it directly invokes (via cli_run args:) or
// indirectly invokes (via a stepWrapperMap step kind).
func scanScenarios(scenarioDir string, leaves []string) (map[string]bool, error) {
	leafSet := make(map[string]bool, len(leaves))
	for _, l := range leaves {
		leafSet[l] = true
	}
	covered := make(map[string]bool)
	argTokenOK := regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

	err := filepath.WalkDir(scenarioDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		var doc any
		if uerr := yaml.Unmarshal(raw, &doc); uerr != nil {
			// Don't fail the whole gate on one malformed
			// scenario; the lint command catches those.
			return nil
		}
		walkScenario(doc, leafSet, covered, argTokenOK)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return covered, nil
}

func walkScenario(node any, leafSet, covered map[string]bool, tok *regexp.Regexp) {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			if k == "args" || k == "argv" {
				if list, ok := v.([]any); ok {
					if leaf := argsToLeaf(list, leafSet, tok); leaf != "" {
						covered[leaf] = true
					}
				}
			}
			if wraps, ok := stepWrapperMap[k]; ok {
				for _, w := range wraps {
					if leafSet[w] {
						covered[w] = true
					}
				}
			}
			walkScenario(v, leafSet, covered, tok)
		}
	case []any:
		for _, v := range n {
			walkScenario(v, leafSet, covered, tok)
		}
	}
}

// argsToLeaf walks an args:[...] list, taking the leading run of
// command-like tokens, and matches the longest prefix that exists
// in the cobra leaf set.  Returns "" if no leading run matches.
func argsToLeaf(list []any, leafSet map[string]bool, tok *regexp.Regexp) string {
	var run []string
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			break
		}
		// Stop at the first flag / placeholder / path-like token.
		if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "$") ||
			strings.ContainsAny(s, "=/:") {
			break
		}
		if !tok.MatchString(s) {
			break
		}
		run = append(run, s)
	}
	var best string
	for i := 1; i <= len(run); i++ {
		cand := strings.Join(run[:i], " ")
		if leafSet[cand] {
			best = cand
		}
	}
	return best
}

func loadAllowlist(path string) (map[string]bool, error) {
	allow := map[string]bool{}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return allow, nil
		}
		return nil, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		allow[line] = true
	}
	return allow, nil
}
