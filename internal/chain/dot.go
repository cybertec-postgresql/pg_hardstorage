// dot.go — Graphviz DOT renderer for the backup-chain graph.
package chain

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// RenderDOT writes the graph as a Graphviz DOT digraph. The output
// drops straight into `dot -Tsvg` / `dot -Tpng` for visual
// inspection, or into BackupGraph.io / Mermaid Live Editor for
// browser-side rendering (with minor flag tweaks).
//
// Conventions:
//   - One subgraph per chain (root + descendants). Orphans live in a
//     separate "orphans" cluster.
//   - Node shape encodes type: full = double-circle, incremental =
//     box, snapshot = ellipse.
//   - Color encodes status: black = healthy, gray = tombstoned, red =
//     orphan or cycle.
//   - Edge label = "+N MiB unique" when ChainMetrics is populated.
//   - Header carries the deployment + walk metadata so a saved DOT
//     file is forensically self-describing.
func RenderDOT(w io.Writer, g *Graph) error {
	if g == nil {
		return fmt.Errorf("chain: nil Graph")
	}
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "// pg_hardstorage backup-chain graph\n")
	fmt.Fprintf(bw, "// deployment: %s\n", g.Deployment)
	fmt.Fprintf(bw, "// generated:  %s\n", g.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "// walk:       %d ms\n", g.DurationMS)
	fmt.Fprintln(bw, "digraph backup_chain {")
	fmt.Fprintln(bw, `  rankdir=LR;`)
	fmt.Fprintln(bw, `  node [fontname="Helvetica"];`)
	fmt.Fprintln(bw, `  edge [fontname="Helvetica", fontsize=10];`)
	fmt.Fprintln(bw, "")
	fmt.Fprintf(bw, "  label=\"backup_chain — %s — %d nodes — max depth %d\";\n",
		escapeDOTString(g.Deployment), g.TotalNodes, g.MaxChainDepth)
	fmt.Fprintln(bw, `  labelloc="t";`)
	fmt.Fprintln(bw, "")
	for i, root := range g.Roots {
		fmt.Fprintf(bw, "  subgraph cluster_chain_%d {\n", i)
		fmt.Fprintf(bw, "    label=\"chain rooted at %s\";\n",
			escapeDOTString(root.BackupID))
		fmt.Fprintln(bw, `    style=rounded; color=lightgrey;`)
		writeNodeDOT(bw, root)
		writeEdgesDOT(bw, root)
		fmt.Fprintln(bw, "  }")
	}
	if len(g.Orphans) > 0 {
		fmt.Fprintln(bw, "")
		fmt.Fprintln(bw, "  subgraph cluster_orphans {")
		fmt.Fprintln(bw, `    label="orphans (parent missing)";`)
		fmt.Fprintln(bw, `    style=dashed; color=red;`)
		for _, o := range g.Orphans {
			writeOrphanDOT(bw, o)
		}
		fmt.Fprintln(bw, "  }")
	}
	fmt.Fprintln(bw, "}")
	_, err := io.WriteString(w, bw.String())
	return err
}

// writeNodeDOT walks a chain DFS-style and emits each node's
// statement. We avoid iterating g.AllNodes because the cluster
// boundary requires per-chain subgraph membership.
func writeNodeDOT(bw *strings.Builder, n *Node) {
	shape := "circle"
	switch n.Type {
	case "full":
		shape = "doublecircle"
	case "incremental_lsn":
		shape = "box"
	case "snapshot":
		shape = "ellipse"
	}
	color := "black"
	style := "filled"
	fillColor := "white"
	if n.Tombstoned {
		color = "grey"
		fillColor = "lightgrey"
		style = "filled,dashed"
	}
	label := dotNodeLabel(n)
	fmt.Fprintf(bw, "    %q [shape=%s, style=%q, color=%s, fillcolor=%q, label=%q];\n",
		n.BackupID, shape, style, color, fillColor, label)
	for _, c := range n.Children {
		writeNodeDOT(bw, c)
	}
}

// writeEdgesDOT emits the parent → child edges for a chain.
func writeEdgesDOT(bw *strings.Builder, n *Node) {
	for _, c := range n.Children {
		edgeLabel := ""
		if c.Metrics != nil {
			edgeLabel = fmt.Sprintf("+%s unique", humanBytes(c.Metrics.UniqueChunkBytes))
		}
		fmt.Fprintf(bw, "    %q -> %q [label=%q];\n",
			n.BackupID, c.BackupID, edgeLabel)
		writeEdgesDOT(bw, c)
	}
}

// writeOrphanDOT emits an orphan node. Red border, dashed; with a
// "parent: <id> (missing)" annotation.
func writeOrphanDOT(bw *strings.Builder, n *Node) {
	label := fmt.Sprintf("%s\\nparent missing: %s", dotNodeLabel(n), n.ParentBackupID)
	fmt.Fprintf(bw, "    %q [shape=box, color=red, style=\"filled,dashed\", fillcolor=mistyrose, label=%q];\n",
		n.BackupID, label)
}

// dotNodeLabel composes a multi-line label for a node. Encoded with
// "\n" so DOT renders newlines properly.
func dotNodeLabel(n *Node) string {
	var lines []string
	lines = append(lines, n.BackupID)
	lines = append(lines, fmt.Sprintf("type: %s", n.Type))
	lines = append(lines, fmt.Sprintf("TLI %d, %s", n.Timeline, humanBytes(n.LogicalBytes)))
	if n.Metrics != nil {
		lines = append(lines, fmt.Sprintf("unique: %s", humanBytes(n.Metrics.UniqueChunkBytes)))
		if n.Metrics.SharedWithAncestors > 0 {
			lines = append(lines, fmt.Sprintf("shared: %s", humanBytes(n.Metrics.SharedWithAncestorsBytes)))
		}
	}
	return strings.Join(lines, "\\n")
}

// escapeDOTString quotes the operator-supplied bits going into the
// graph header (deployment name). DOT strings allow newlines via
// \n, double-quotes need escaping with backslash.
func escapeDOTString(s string) string {
	r := strings.NewReplacer(
		`"`, `\"`,
		"\n", `\n`,
	)
	return r.Replace(s)
}

// sortedRootsByID returns the graph's roots in stable BackupID
// order. Helper for renderers that want lex-sorted output.
func sortedRootsByID(g *Graph) []*Node {
	out := append([]*Node(nil), g.Roots...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].BackupID < out[j].BackupID
	})
	return out
}
