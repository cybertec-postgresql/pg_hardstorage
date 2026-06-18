// backup_graph.go — CLI surface for the backup-chain graph (tree / markdown / dot views).
package cli

import (
	stdjson "encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/chain"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// newBackupGraphCmd implements `pg_hardstorage backup graph
// <deployment>` — the chain-topology + dedup-analysis report.
//
// Operationally answers:
//
//   - "What's the topology of my backup chains?"
//     → JSON / DOT / ASCII tree output
//
//   - "Which incrementals actually contribute new bytes vs
//     piggy-back on the parent's chunks?"
//     → per-node ChainMetrics (TotalChunks, UniqueChunks,
//     SharedWithAncestors, DedupRatioVsChain)
//
//   - "Are any chains orphaned, broken, or crossing timeline
//     boundaries?"
//     → Issues list with severity + suggestion
//
// Different from:
//   - `backup compare a b`  — diff between two specific manifests
//   - `repo audit`           — fleet-level snapshot of FACTS
//   - `recovery readiness`   — recovery posture
//   - `backup graph`         — chain topology + dedup contribution
//
// Read-only by construction; safe at any cadence.
func newBackupGraphCmd() *cobra.Command {
	var (
		repoURL           string
		format            string
		includeTombstoned bool
		skipAnalysis      bool
	)
	c := &cobra.Command{
		Use:   "graph <deployment>",
		Short: "Backup-chain topology + per-node dedup analysis",
		Long: `Walks every visible manifest for the deployment, links them by
parent_backup_id into a chain forest, and (by default) computes
per-node chunk-overlap metrics: how many chunks this backup
contributes vs how many it shares with ancestors in the chain.

Output formats:
  --format json     (default) — JSON body; the v1 contract.
  --format markdown — forensics-grade GFM with per-chain section,
                      ASCII tree, per-node detail table, issues.
  --format dot      — Graphviz DOT digraph. Pipe through
                      ` + "`dot -Tsvg`" + ` or ` + "`dot -Tpng`" + ` to render.
  --format tree     — pure ASCII tree (suitable for terminal
                      output and pipe-to-grep workflows).

Flags:
  --include-tombstoned  include soft-deleted manifests in the
                        graph (marked with "tombstoned").
                        Default: live view only.
  --no-analysis         skip the chunk-overlap pass. Useful on
                        very large chains where memory is tight.
                        Per-node Metrics will be nil.

Read-only; safe at any cadence.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupGraph(cmd, args[0], backupGraphFlags{
				repoURL:           repoURL,
				format:            format,
				includeTombstoned: includeTombstoned,
				skipAnalysis:      skipAnalysis,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&format, "format", "json",
		"output format: json | markdown | dot | tree")
	c.Flags().BoolVar(&includeTombstoned, "include-tombstoned", false,
		"include soft-deleted manifests in the graph")
	c.Flags().BoolVar(&skipAnalysis, "no-analysis", false,
		"skip the chunk-overlap analysis pass (faster, less memory)")
	return c
}

type backupGraphFlags struct {
	repoURL           string
	format            string
	includeTombstoned bool
	skipAnalysis      bool
}

func runBackupGraph(cmd *cobra.Command, deployment string, f backupGraphFlags) error {
	d := DispatcherFrom(cmd)
	switch f.format {
	case "", "json", "markdown", "dot", "tree":
	default:
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("backup graph: --format must be json | markdown | dot | tree; got %q", f.format)).
			Wrap(output.ErrUsage)
	}

	verifier, err := loadVerifier()
	if err != nil {
		return err
	}
	_, sp, err := openRepo(cmd.Context(), f.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	g, err := chain.BuildGraph(cmd.Context(), sp, deployment, chain.Options{
		Verifier:          verifier,
		IncludeTombstoned: f.includeTombstoned,
		SkipAnalysis:      f.skipAnalysis,
	})
	if err != nil {
		return output.NewError("backup.graph_failed",
			fmt.Sprintf("backup graph: %v", err)).Wrap(err)
	}
	g.URL = f.repoURL

	body := backupGraphBody{Graph: g, format: f.format}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// backupGraphBody wraps chain.Graph with renderer hooks for the
// dual JSON / Markdown / DOT / tree output flow. JSON always emits
// the underlying Graph verbatim via MarshalJSON so consumers see
// only the v1 contract.
type backupGraphBody struct {
	*chain.Graph
	format string
}

// MarshalJSON emits the embedded chain.Graph verbatim so JSON consumers see
// only the v1 contract.
func (b backupGraphBody) MarshalJSON() ([]byte, error) {
	return stdjson.Marshal(b.Graph)
}

// WriteText routes by format. The compact text view (no
// --format) is the same as `tree` — operators eyeballing a chain
// don't want a 30-line metadata header.
func (b backupGraphBody) WriteText(w io.Writer) error {
	switch strings.ToLower(b.format) {
	case "markdown":
		return chain.RenderMarkdown(w, b.Graph)
	case "dot":
		return chain.RenderDOT(w, b.Graph)
	default:
		return chain.RenderTree(w, b.Graph)
	}
}

// newBackupGraphCmd is wired into the existing backup parent in
// internal/cli/backup.go via newRealBackupCmd. Cross-file: backup.go
// already adds compare / delete / undelete / etc. — the wiring for
// graph lands there.
