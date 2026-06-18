// tree.go — Unicode-box-drawing ASCII tree renderer for the backup-chain graph.
package chain

import (
	"fmt"
	"io"
	"strings"
)

// RenderTree writes the graph as a Unicode-box-drawing ASCII tree.
// One section per chain root + a separate "Orphans" section. Per-
// node lines show: BackupID + type + size + (when analyzed) unique
// vs shared chunk bytes.
//
// Designed for fixed-width terminals; the box-drawing characters
// (├─, └─, │) are the same set every modern terminal renders.
func RenderTree(w io.Writer, g *Graph) error {
	if g == nil {
		return fmt.Errorf("chain: nil Graph")
	}
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "backup chain — deployment %s — %d nodes (full %d / incremental %d / snapshot %d)\n",
		g.Deployment, g.TotalNodes, g.FullCount, g.IncrementalCount, g.SnapshotCount)
	fmt.Fprintf(bw, "max depth: %d   roots: %d   orphans: %d\n\n",
		g.MaxChainDepth, len(g.Roots), len(g.Orphans))

	if len(g.Roots) == 0 && len(g.Orphans) == 0 {
		fmt.Fprintln(bw, "(no backups for this deployment)")
		_, err := io.WriteString(w, bw.String())
		return err
	}

	for i, root := range g.Roots {
		if i > 0 {
			fmt.Fprintln(bw)
		}
		fmt.Fprintf(bw, "● %s\n", treeNodeLabel(root))
		writeChildrenTree(bw, root, "")
	}

	if len(g.Orphans) > 0 {
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "Orphans (parent_backup_id refers to a manifest not in the visible set):")
		for _, o := range g.Orphans {
			fmt.Fprintf(bw, "  ✗ %s — parent_backup_id=%s (missing)\n",
				treeNodeLabel(o), o.ParentBackupID)
		}
	}

	if len(g.Issues) > 0 {
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "Issues:")
		for _, iss := range g.Issues {
			fmt.Fprintf(bw, "  [%s] %s — %s\n", iss.Severity, iss.Code, iss.Message)
		}
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

// writeChildrenTree emits the node's children under the given
// prefix. Last-child gets └─; others get ├─; continuations use │.
func writeChildrenTree(bw *strings.Builder, n *Node, prefix string) {
	for i, c := range n.Children {
		isLast := i == len(n.Children)-1
		var connector, childPrefix string
		if isLast {
			connector = "└─"
			childPrefix = prefix + "   "
		} else {
			connector = "├─"
			childPrefix = prefix + "│  "
		}
		fmt.Fprintf(bw, "%s%s %s\n", prefix, connector, treeNodeLabel(c))
		writeChildrenTree(bw, c, childPrefix)
	}
}

// treeNodeLabel composes a single line for the tree. Includes
// the type tag, logical bytes, and (when analyzed) the unique vs
// shared bytes.
func treeNodeLabel(n *Node) string {
	parts := []string{n.BackupID}
	tag := fmt.Sprintf("[%s]", n.Type)
	if n.Tombstoned {
		tag += " (tombstoned)"
	}
	parts = append(parts, tag, fmt.Sprintf("%s logical", humanBytes(n.LogicalBytes)))
	if n.Metrics != nil {
		parts = append(parts, fmt.Sprintf("unique=%s", humanBytes(n.Metrics.UniqueChunkBytes)))
		if n.Metrics.SharedWithAncestors > 0 {
			parts = append(parts, fmt.Sprintf("shared=%s", humanBytes(n.Metrics.SharedWithAncestorsBytes)))
		}
	}
	if n.Encrypted {
		parts = append(parts, "encrypted")
	}
	return strings.Join(parts, "  ")
}
