// markdown.go — GFM Markdown renderer for the backup-chain graph (forensics-grade report).
package chain

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// RenderMarkdown writes the graph as a forensics-grade GFM document
// matching the discipline of the compliance / forecast / recovery
// renderers. Sections in fixed order:
//
//  1. Header (metadata table + design note)
//  2. Summary (counts + max depth)
//  3. Per-chain detail (one section per root: ASCII tree +
//     per-node metrics table + ChainSummary headline)
//  4. Orphans (table)
//  5. Issues (table sorted by severity)
//
// GFM tables for everything tabular; ✓/·/✗ glyphs on headlines.
func RenderMarkdown(w io.Writer, g *Graph) error {
	if g == nil {
		return fmt.Errorf("chain: nil Graph")
	}
	bw := &strings.Builder{}
	writeMDHeader(bw, g)
	writeMDSummary(bw, g)
	writeMDChains(bw, g)
	writeMDOrphans(bw, g)
	writeMDIssues(bw, g)
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n")+"\n")
	return err
}

func writeMDHeader(bw *strings.Builder, g *Graph) {
	fmt.Fprintf(bw, "# pg_hardstorage backup chain — `%s`\n\n", g.Deployment)
	fmt.Fprintln(bw, "| Field | Value |")
	fmt.Fprintln(bw, "| --- | --- |")
	if g.URL != "" {
		fmt.Fprintf(bw, "| Repository | `%s` |\n", g.URL)
	}
	fmt.Fprintf(bw, "| Deployment | `%s` |\n", g.Deployment)
	fmt.Fprintf(bw, "| Total nodes | %d |\n", g.TotalNodes)
	fmt.Fprintf(bw, "| Generated at | %s |\n", g.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "| Walk duration | %d ms |\n", g.DurationMS)
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "_The chain graph + per-node dedup analysis. \"Unique\" chunks are those NOT present in any ancestor in the chain; \"shared\" are those that piggy-back on the parent's chunks. Read-only; safe at any cadence._")
	fmt.Fprintln(bw)
}

func writeMDSummary(bw *strings.Builder, g *Graph) {
	fmt.Fprintln(bw, "## Summary")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "| Metric | Value |")
	fmt.Fprintln(bw, "| --- | --- |")
	fmt.Fprintf(bw, "| Roots | %d |\n", len(g.Roots))
	fmt.Fprintf(bw, "| Full backups | %d |\n", g.FullCount)
	fmt.Fprintf(bw, "| Incrementals | %d |\n", g.IncrementalCount)
	if g.SnapshotCount > 0 {
		fmt.Fprintf(bw, "| Snapshots | %d |\n", g.SnapshotCount)
	}
	fmt.Fprintf(bw, "| Max chain depth | %d |\n", g.MaxChainDepth)
	if g.OrphanCount > 0 {
		fmt.Fprintf(bw, "| ✗ Orphans | %d |\n", g.OrphanCount)
	}
	fmt.Fprintln(bw)
}

func writeMDChains(bw *strings.Builder, g *Graph) {
	fmt.Fprintln(bw, "## Chains")
	fmt.Fprintln(bw)
	if len(g.Roots) == 0 {
		fmt.Fprintln(bw, "_(no chains)_")
		fmt.Fprintln(bw)
		return
	}
	for _, root := range g.Roots {
		summary := SummarizeChain(root)
		fmt.Fprintf(bw, "### `%s`\n\n", root.BackupID)
		fmt.Fprintln(bw, "| Field | Value |")
		fmt.Fprintln(bw, "| --- | --- |")
		fmt.Fprintf(bw, "| Nodes | %d |\n", summary.NodeCount)
		fmt.Fprintf(bw, "| Max depth | %d |\n", summary.MaxDepth)
		fmt.Fprintf(bw, "| Leaves | %d |\n", summary.LeafCount)
		fmt.Fprintf(bw, "| Logical bytes (sum across chain) | %s |\n",
			humanBytes(summary.LogicalBytesSum))
		if summary.UniqueChunkBytesSum > 0 {
			fmt.Fprintf(bw, "| Unique chunk bytes (sum) | %s |\n",
				humanBytes(summary.UniqueChunkBytesSum))
			fmt.Fprintf(bw, "| Shared chunk bytes (sum) | %s |\n",
				humanBytes(summary.SharedChunkBytesSum))
			fmt.Fprintf(bw, "| Chain dedup ratio | %.2fx |\n", summary.DedupRatioOverall)
		}
		fmt.Fprintln(bw)

		fmt.Fprintln(bw, "**Tree:**")
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "```")
		fmt.Fprintf(bw, "● %s\n", treeNodeLabel(root))
		var sb strings.Builder
		writeChildrenTree(&sb, root, "")
		fmt.Fprint(bw, sb.String())
		fmt.Fprintln(bw, "```")
		fmt.Fprintln(bw)

		fmt.Fprintln(bw, "**Per-node detail:**")
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "| Backup | Type | Stopped at | TLI | Logical | Total chunks | Unique | Shared | Dedup |")
		fmt.Fprintln(bw, "| --- | --- | --- | --- | --- | --- | --- | --- | --- |")
		writeChainNodeRows(bw, root)
		fmt.Fprintln(bw)
	}
}

func writeChainNodeRows(bw *strings.Builder, n *Node) {
	logical := humanBytes(n.LogicalBytes)
	var totalChunks, unique, shared, dedup string
	if n.Metrics != nil {
		totalChunks = fmt.Sprintf("%d", n.Metrics.TotalChunks)
		unique = fmt.Sprintf("%d (%s)", n.Metrics.UniqueChunks,
			humanBytes(n.Metrics.UniqueChunkBytes))
		shared = fmt.Sprintf("%d (%s)", n.Metrics.SharedWithAncestors,
			humanBytes(n.Metrics.SharedWithAncestorsBytes))
		if n.Metrics.DedupRatioVsChain > 0 {
			dedup = fmt.Sprintf("%.2fx", n.Metrics.DedupRatioVsChain)
		} else {
			dedup = "—"
		}
	} else {
		totalChunks, unique, shared, dedup = "—", "—", "—", "—"
	}
	fmt.Fprintf(bw, "| `%s` | %s | %s | %d | %s | %s | %s | %s | %s |\n",
		n.BackupID, n.Type, n.StoppedAt.Format(time.RFC3339), n.Timeline,
		logical, totalChunks, unique, shared, dedup)
	for _, c := range n.Children {
		writeChainNodeRows(bw, c)
	}
}

func writeMDOrphans(bw *strings.Builder, g *Graph) {
	if len(g.Orphans) == 0 {
		return
	}
	fmt.Fprintln(bw, "## ✗ Orphans")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "_These manifests reference a `parent_backup_id` that is NOT in the visible set. Restore from an orphan is not possible without its parent._")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "| Backup | Missing parent | Type | Stopped at | TLI |")
	fmt.Fprintln(bw, "| --- | --- | --- | --- | --- |")
	for _, o := range g.Orphans {
		fmt.Fprintf(bw, "| `%s` | `%s` | %s | %s | %d |\n",
			o.BackupID, o.ParentBackupID, o.Type,
			o.StoppedAt.Format(time.RFC3339), o.Timeline)
	}
	fmt.Fprintln(bw)
}

func writeMDIssues(bw *strings.Builder, g *Graph) {
	if len(g.Issues) == 0 {
		return
	}
	fmt.Fprintln(bw, "## Issues")
	fmt.Fprintln(bw)
	issues := append([]GraphIssue(nil), g.Issues...)
	sort.SliceStable(issues, func(i, j int) bool {
		return severityWeight(issues[i].Severity) < severityWeight(issues[j].Severity)
	})
	fmt.Fprintln(bw, "| Severity | Code | Backup | Message | Suggestion |")
	fmt.Fprintln(bw, "| --- | --- | --- | --- | --- |")
	for _, iss := range issues {
		bid := "—"
		if iss.BackupID != "" {
			bid = "`" + iss.BackupID + "`"
		}
		fmt.Fprintf(bw, "| %s | `%s` | %s | %s | %s |\n",
			iss.Severity, iss.Code, bid, iss.Message,
			fallbackOrDash(iss.Suggestion))
	}
	fmt.Fprintln(bw)
}

func severityWeight(s string) int {
	switch s {
	case "critical":
		return 0
	case "warning":
		return 1
	case "notice":
		return 2
	}
	return 3
}

func fallbackOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// humanBytes mirrors the implementation in forecast / repoaudit /
// recovery. Duplicated to avoid cross-package imports for a
// presentation helper.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	suffix := "KMGTPE"[exp]
	return fmt.Sprintf("%.2f %ciB", float64(b)/float64(div), suffix)
}
