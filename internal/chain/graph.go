// Package chain implements the backup-chain graph + dedup-analysis
// surfaces behind `pg_hardstorage backup graph`.
//
// PG 17 incremental backups + the existing FastCDC chunker produce
// backup chains: a `full` anchor and one or more `incremental_lsn`
// children that reference the parent via `ParentBackupID`. At scale
// (deployments with many incrementals over weeks of operation),
// operators need to answer:
//
//   - "What's the topology of my backup chains?"
//     → graph view, ASCII tree or Graphviz DOT
//
//   - "Which backups actually contribute new bytes vs piggy-back
//     on the parent's chunks?"
//     → per-node dedup analysis: TotalChunks, UniqueChunks
//     (not present in any ancestor), SharedWithAncestors
//
//   - "Are any chains orphaned (incremental references a parent
//     that's been deleted) or broken (cycle, missing root)?"
//     → integrity findings with severity + suggestion
//
// Read-only by construction. Walks the manifest store; never
// mutates anything. Safe at any cadence.
package chain

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// GraphSchema is the on-disk version tag for Graph bodies.
const GraphSchema = "pg_hardstorage.backup_chain.v1"

// Graph is the structured chain forest. A "graph" here is actually
// a forest — operators usually have multiple full backups in a
// deployment, each anchoring its own chain.
type Graph struct {
	Schema      string    `json:"schema"`
	GeneratedAt time.Time `json:"generated_at"`
	StoppedAt   time.Time `json:"stopped_at"`
	DurationMS  int64     `json:"duration_ms"`

	URL        string `json:"url,omitempty"`
	Deployment string `json:"deployment"`

	// Roots are full backups (or snapshot anchors) with at least
	// one descendant — i.e., heads of incremental chains. A
	// deployment with no incrementals has every full as a root
	// with zero children.
	Roots []*Node `json:"roots"`

	// Orphans are incrementals whose parent_backup_id refers to a
	// manifest that's not visible (deleted / tombstoned / never
	// existed). These can't be restored without their chain;
	// surfaced loudly so the operator can decide.
	Orphans []*Node `json:"orphans,omitempty"`

	// AllNodes is the flat list of every node, sorted by BackupID.
	// Useful for renderers that walk by ID rather than by tree
	// position. NOT serialized in JSON — the Roots + Orphans
	// slices already cover every node, and a flat list with
	// pointer-back to children would create JSON cycles.
	AllNodes []*Node `json:"-"`

	// Counters: aggregate over the entire graph.
	TotalNodes       int `json:"total_nodes"`
	FullCount        int `json:"full_count"`
	IncrementalCount int `json:"incremental_count"`
	SnapshotCount    int `json:"snapshot_count,omitempty"`
	OrphanCount      int `json:"orphan_count,omitempty"`

	// MaxChainDepth is the longest chain in the graph (root→leaf).
	// 1 = full only; 2 = full + 1 incremental; etc.
	MaxChainDepth int `json:"max_chain_depth"`

	// Issues are integrity findings: orphans, missing parents,
	// cycles, mixed timelines, etc.
	Issues []GraphIssue `json:"issues,omitempty"`
}

// Node is one backup in the graph + its position metadata.
type Node struct {
	BackupID       string    `json:"backup_id"`
	Deployment     string    `json:"deployment"`
	Type           string    `json:"type"`
	ParentBackupID string    `json:"parent_backup_id,omitempty"`
	StoppedAt      time.Time `json:"stopped_at"`
	StartLSN       string    `json:"start_lsn,omitempty"`
	StopLSN        string    `json:"stop_lsn,omitempty"`
	Timeline       uint32    `json:"timeline"`
	PGVersion      int       `json:"pg_version,omitempty"`

	// Encrypted flags whether this manifest carries an Encryption
	// block. KEKRef is set when Encrypted==true.
	Encrypted bool   `json:"encrypted"`
	KEKRef    string `json:"kek_ref,omitempty"`

	// Tombstoned marks soft-deleted manifests included in the
	// graph (only when --include-tombstoned). Renderers display
	// them dashed; analysis still walks them but flags them.
	Tombstoned bool `json:"tombstoned,omitempty"`

	// LogicalBytes = sum of FileEntry.Size for this manifest.
	LogicalBytes int64 `json:"logical_bytes"`

	// Depth is 1 for roots, 2 for direct children, etc.
	Depth int `json:"depth"`

	// Parent is the parent Node when present (nil for roots).
	// NOT serialized to avoid JSON cycles.
	Parent *Node `json:"-"`

	// Children are the direct descendants. Sorted by StoppedAt.
	Children []*Node `json:"children,omitempty"`

	// Metrics carries chunk-overlap analysis. Populated by
	// AnalyzeChain; nil before analysis.
	Metrics *ChainMetrics `json:"metrics,omitempty"`

	// chunks is the per-node distinct chunk set (hash → length),
	// stashed at build time from the manifest. Private — analysis
	// reads it directly; renderers don't need it. Not serialized.
	chunks map[repo.Hash]int64 `json:"-"`
}

// IsRoot reports whether this node sits at the top of a chain.
func (n *Node) IsRoot() bool { return n.Parent == nil && n.ParentBackupID == "" }

// IsLeaf reports whether this node has no children.
func (n *Node) IsLeaf() bool { return len(n.Children) == 0 }

// IsOrphan reports whether this node refers to a parent that
// isn't in the graph.
func (n *Node) IsOrphan() bool { return n.Parent == nil && n.ParentBackupID != "" }

// GraphIssue is one integrity finding.
type GraphIssue struct {
	Severity   string `json:"severity"` // "critical" | "warning" | "notice"
	Code       string `json:"code"`
	Message    string `json:"message"`
	BackupID   string `json:"backup_id,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

// Options configures one BuildGraph run.
type Options struct {
	// Verifier validates each manifest's signature. Required.
	Verifier *backup.Verifier

	// Now overrides time.Now() for deterministic test output.
	Now time.Time

	// IncludeTombstoned, when true, includes soft-deleted
	// manifests in the graph (marked Tombstoned). Default false:
	// the live recovery view excludes them.
	IncludeTombstoned bool

	// SkipAnalysis suppresses the chunk-overlap pass. Useful on
	// huge chains where the O(unique chunks across chain) memory
	// cost is unwelcome.
	SkipAnalysis bool
}

// BuildGraph walks every visible manifest for the deployment and
// constructs the chain forest. Performs analysis unless
// opts.SkipAnalysis.
func BuildGraph(ctx context.Context, sp storage.StoragePlugin, deployment string, opts Options) (*Graph, error) {
	if sp == nil {
		return nil, errors.New("chain: nil StoragePlugin")
	}
	if opts.Verifier == nil {
		return nil, errors.New("chain: Verifier is required")
	}
	if deployment == "" {
		return nil, errors.New("chain: deployment is required")
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	started := time.Now().UTC()
	g := &Graph{
		Schema:      GraphSchema,
		GeneratedAt: now,
		Deployment:  deployment,
	}
	finish := func() {
		g.StoppedAt = time.Now().UTC()
		g.DurationMS = g.StoppedAt.Sub(started).Milliseconds()
	}

	store := backup.NewManifestStore(sp)

	byID := map[string]*Node{}
	if opts.IncludeTombstoned {
		for entry, lerr := range store.ListIncludingTombstoned(ctx, deployment, opts.Verifier) {
			if err := ctx.Err(); err != nil {
				finish()
				return g, err
			}
			if lerr != nil {
				continue
			}
			n := manifestToNode(entry.Manifest, opts.IncludeTombstoned)
			n.Tombstoned = entry.Tombstoned
			byID[n.BackupID] = n
		}
	} else {
		for m, lerr := range store.List(ctx, deployment, opts.Verifier) {
			if err := ctx.Err(); err != nil {
				finish()
				return g, err
			}
			if lerr != nil {
				continue
			}
			n := manifestToNode(m, false)
			byID[n.BackupID] = n
		}
	}

	// Wire parent/child links.
	for _, n := range byID {
		if n.ParentBackupID == "" {
			continue
		}
		parent, ok := byID[n.ParentBackupID]
		if !ok {
			// Orphan — parent missing from the visible set.
			continue
		}
		n.Parent = parent
		parent.Children = append(parent.Children, n)
	}

	// Detect + break cycles via a DFS coloring. A cycle in the
	// parent_backup_id chain shouldn't be possible (the commit
	// path validates), but we surface findings if one occurs and
	// drop the back-edge so the rest of the graph is sane.
	cycleNodes := detectCycles(byID)
	for id := range cycleNodes {
		// Break cycle by clearing the parent link of the cycle's
		// "earliest" member (deterministic break point).
		n := byID[id]
		if n.Parent != nil {
			n.Parent.Children = removeChild(n.Parent.Children, n)
			n.Parent = nil
		}
		g.Issues = append(g.Issues, GraphIssue{
			Severity:   "critical",
			Code:       "chain.cycle_detected",
			BackupID:   id,
			Message:    fmt.Sprintf("backup %q participates in a parent_backup_id cycle; the back-edge has been dropped", id),
			Suggestion: "manually inspect the manifests; the manifest commit path should never produce a cycle",
		})
	}

	// Sort children deterministically by StoppedAt (oldest first
	// within a chain — operators read the chain top-down by time).
	for _, n := range byID {
		sort.Slice(n.Children, func(i, j int) bool {
			return n.Children[i].StoppedAt.Before(n.Children[j].StoppedAt)
		})
	}

	// Compute Depth via DFS from each root.
	for _, n := range byID {
		if n.IsRoot() {
			assignDepth(n, 1)
		}
	}

	// Materialise Roots, Orphans, and AllNodes.
	for _, n := range byID {
		switch {
		case n.IsRoot():
			g.Roots = append(g.Roots, n)
		case n.IsOrphan():
			g.Orphans = append(g.Orphans, n)
			g.OrphanCount++
		}
		g.AllNodes = append(g.AllNodes, n)
		g.TotalNodes++
		switch n.Type {
		case string(backup.BackupTypeFull):
			g.FullCount++
		case string(backup.BackupTypeIncremental):
			g.IncrementalCount++
		case string(backup.BackupTypeSnapshot):
			g.SnapshotCount++
		}
		if n.Depth > g.MaxChainDepth {
			g.MaxChainDepth = n.Depth
		}
	}
	sort.Slice(g.Roots, func(i, j int) bool {
		return g.Roots[i].StoppedAt.Before(g.Roots[j].StoppedAt)
	})
	sort.Slice(g.Orphans, func(i, j int) bool {
		return g.Orphans[i].BackupID < g.Orphans[j].BackupID
	})
	sort.Slice(g.AllNodes, func(i, j int) bool {
		return g.AllNodes[i].BackupID < g.AllNodes[j].BackupID
	})

	// Surface orphan issues.
	for _, n := range g.Orphans {
		g.Issues = append(g.Issues, GraphIssue{
			Severity:   "critical",
			Code:       "chain.orphaned_incremental",
			BackupID:   n.BackupID,
			Message:    fmt.Sprintf("incremental %q references parent %q which is not present in the visible set", n.BackupID, n.ParentBackupID),
			Suggestion: "the parent has been deleted (or never committed); restore from this orphan is not possible without the parent. Check `list --include-deleted` or `repo audit` for the parent's status",
		})
	}

	// Mixed-timeline detection within a chain. PG promotes increment
	// the timeline; an incremental with a different timeline than its
	// parent is benign metadata-wise but worth surfacing — the
	// operator should know they crossed a failover boundary.
	for _, n := range byID {
		if n.Parent != nil && n.Timeline != n.Parent.Timeline {
			g.Issues = append(g.Issues, GraphIssue{
				Severity: "notice",
				Code:     "chain.timeline_advance",
				BackupID: n.BackupID,
				Message: fmt.Sprintf("incremental %q is on TLI %d; parent %q is on TLI %d (Patroni promotion crossed the chain)",
					n.BackupID, n.Timeline, n.ParentBackupID, n.Parent.Timeline),
				Suggestion: "this is informational; the chain is valid, the timeline advance is recorded in `wal/<deployment>/timelines/`",
			})
		}
	}

	if !opts.SkipAnalysis {
		AnalyzeChain(g)
	}

	finish()
	return g, nil
}

// manifestToNode shapes one Manifest into a Node. Caller wires the
// parent link in a second pass.
func manifestToNode(m *backup.Manifest, _ bool) *Node {
	n := &Node{
		BackupID:       m.BackupID,
		Deployment:     m.Deployment,
		Type:           string(m.Type),
		ParentBackupID: m.ParentBackupID,
		StoppedAt:      m.StoppedAt,
		StartLSN:       m.StartLSN,
		StopLSN:        m.StopLSN,
		Timeline:       m.Timeline,
		PGVersion:      m.PGVersion,
		LogicalBytes:   sumFileSizes(m),
	}
	if m.Encryption != nil {
		n.Encrypted = true
		n.KEKRef = m.Encryption.KEKRef
	}
	n.chunks = uniqueChunkSet(m)
	return n
}

// sumFileSizes returns the manifest's logical bytes — same definition
// as forecast / repoaudit.
func sumFileSizes(m *backup.Manifest) int64 {
	var total int64
	for _, f := range m.Files {
		total += f.Size
	}
	return total
}

// assignDepth fills Depth for n and its children via DFS.
func assignDepth(n *Node, d int) {
	n.Depth = d
	for _, c := range n.Children {
		assignDepth(c, d+1)
	}
}

// removeChild returns the slice with n removed. Used when breaking
// cycles. Stable order otherwise.
func removeChild(children []*Node, target *Node) []*Node {
	out := children[:0]
	for _, c := range children {
		if c != target {
			out = append(out, c)
		}
	}
	return out
}

// detectCycles returns the set of node IDs that participate in a
// parent_backup_id cycle. The first node visited in a cycle is
// returned as the cycle's representative (used by the caller to
// pick which back-edge to drop). Implementation: DFS coloring.
//
// White = unvisited, Gray = on the current DFS stack, Black = done.
// A child edge to a Gray node = cycle detected.
func detectCycles(nodes map[string]*Node) map[string]struct{} {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	cycles := map[string]struct{}{}
	var visit func(id string)
	visit = func(id string) {
		n, ok := nodes[id]
		if !ok {
			return
		}
		switch color[id] {
		case gray:
			cycles[id] = struct{}{}
			return
		case black:
			return
		}
		color[id] = gray
		if n.ParentBackupID != "" {
			visit(n.ParentBackupID)
		}
		color[id] = black
	}
	for id := range nodes {
		if color[id] == white {
			visit(id)
		}
	}
	return cycles
}

// uniqueChunkSet returns the set of distinct chunk hashes in a
// manifest. A chunk referenced by multiple files counts once.
func uniqueChunkSet(m *backup.Manifest) map[repo.Hash]int64 {
	out := map[repo.Hash]int64{}
	for _, f := range m.Files {
		for _, c := range f.Chunks {
			// Multiple references to the same chunk inside a
			// single manifest happen for tablespace shared
			// chunks. We keep the first observed length.
			if _, ok := out[c.Hash]; !ok {
				out[c.Hash] = c.Len
			}
		}
	}
	return out
}
