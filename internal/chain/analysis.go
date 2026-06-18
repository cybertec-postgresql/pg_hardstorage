// analysis.go — ChainMetrics: per-node chunk-overlap accounting across ancestor backups.
package chain

import (
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// ChainMetrics records the per-node chunk-overlap analysis. It
// answers: "how much does THIS backup actually contribute over
// what its ancestors already had in the chain?".
//
// The numbers below are computed against the manifest as committed,
// not against on-storage state. A chunk that's been GC'd is still
// counted here as "referenced"; the on-disk integrity story is
// `verify` / `repo scrub` / `kms verify`'s job.
type ChainMetrics struct {
	// TotalChunks is the count of distinct chunk hashes in this
	// manifest (a hash referenced by N files counts once).
	TotalChunks int `json:"total_chunks"`

	// TotalChunkBytes sums the chunk lengths for the distinct set.
	// (NOT the same as LogicalBytes — that's FileEntry.Size; this
	// is the chunk-level total which can differ when files share
	// chunks).
	TotalChunkBytes int64 `json:"total_chunk_bytes"`

	// UniqueChunks is the count of distinct chunk hashes in this
	// manifest that DO NOT appear in any ancestor in the chain.
	// For a root, UniqueChunks == TotalChunks.
	UniqueChunks int `json:"unique_chunks"`

	// UniqueChunkBytes is the byte sum of UniqueChunks.
	UniqueChunkBytes int64 `json:"unique_chunk_bytes"`

	// SharedWithAncestors is the count of distinct chunk hashes
	// in this manifest that ALREADY appeared in an ancestor.
	// For a root, SharedWithAncestors == 0.
	SharedWithAncestors int `json:"shared_with_ancestors"`

	// SharedWithAncestorsBytes is the byte sum of
	// SharedWithAncestors.
	SharedWithAncestorsBytes int64 `json:"shared_with_ancestors_bytes"`

	// DedupRatioVsChain is TotalChunks / UniqueChunks (when
	// UniqueChunks > 0). A value of 1.0 means "every chunk is
	// new" (no dedup-with-ancestors); higher values mean
	// "incremental's bytes mostly already existed in the chain".
	DedupRatioVsChain float64 `json:"dedup_ratio_vs_chain,omitempty"`

	// AncestorChunkCount is the size of the "chunks present in
	// any ancestor" set this node was compared against. Cached
	// here so renderers can show "this incremental was tested
	// against N ancestor chunks".
	AncestorChunkCount int `json:"ancestor_chunk_count,omitempty"`
}

// ChainSummary aggregates metrics across a single chain (root +
// all descendants). Useful for the per-chain section in the
// Markdown rendering.
type ChainSummary struct {
	RootBackupID string `json:"root_backup_id"`

	// NodeCount is the total number of nodes in this chain
	// (including the root).
	NodeCount int `json:"node_count"`

	// LogicalBytesSum is the sum of LogicalBytes across the
	// whole chain. For incrementals this overstates "data
	// stored on the source"; the per-node UniqueChunkBytes
	// tells the dedup story.
	LogicalBytesSum int64 `json:"logical_bytes_sum"`

	// UniqueChunkBytesSum is the sum of UniqueChunkBytes across
	// the chain. This IS roughly "unique bytes stored on
	// storage" for the chain (modulo cross-chain dedup, which
	// is fleet-level and not in scope here).
	UniqueChunkBytesSum int64 `json:"unique_chunk_bytes_sum"`

	// SharedChunkBytesSum is the sum of SharedWithAncestorsBytes
	// across the chain. Equivalently: how many bytes did
	// incrementals "save" by referencing parent chunks.
	SharedChunkBytesSum int64 `json:"shared_chunk_bytes_sum"`

	// DedupRatioOverall is LogicalBytesSum /
	// UniqueChunkBytesSum (when the latter is non-zero). A
	// chain with no incrementals reports ratio 1.0; a
	// well-deduping chain reports much higher.
	DedupRatioOverall float64 `json:"dedup_ratio_overall,omitempty"`

	// MaxDepth is the depth of the deepest leaf below this root.
	MaxDepth int `json:"max_depth"`

	// LeafCount is the number of leaf nodes (no children) in
	// this chain.
	LeafCount int `json:"leaf_count"`
}

// AnalyzeChain populates Node.Metrics for every node in the graph
// AND fills the per-chain summaries. Walks each chain top-down,
// extending an "ancestor chunks" set at every step.
//
// Memory: O(total distinct chunks across each chain). For chains
// with millions of chunks this can dominate; the BuildGraph caller
// can opt out via Options.SkipAnalysis.
//
// We do NOT walk orphans; their parent set is unknown so the
// metrics would be misleading.
func AnalyzeChain(g *Graph) {
	for _, root := range g.Roots {
		analyzeNode(root, map[repo.Hash]int64{})
	}
}

// analyzeNode is the per-node analyzer. ancestors is the
// (chunk-hash → length) map of every chunk seen in any strict
// ancestor of n. n's own chunks are added to the next-level
// ancestor map before recursing into children.
//
// We pass the ancestors map by value (i.e., we copy it before
// extending) so siblings see the same starting set rather than
// each other's chunks. Without this, a depth-2 fan-out would
// alias the parent's pointer and double-count.
func analyzeNode(n *Node, ancestors map[repo.Hash]int64) {
	own := n.chunks
	m := &ChainMetrics{
		TotalChunks:        len(own),
		AncestorChunkCount: len(ancestors),
	}
	for h, l := range own {
		m.TotalChunkBytes += l
		if _, inAncestor := ancestors[h]; inAncestor {
			m.SharedWithAncestors++
			m.SharedWithAncestorsBytes += l
		} else {
			m.UniqueChunks++
			m.UniqueChunkBytes += l
		}
	}
	if m.UniqueChunks > 0 {
		m.DedupRatioVsChain = float64(m.TotalChunks) / float64(m.UniqueChunks)
	}
	n.Metrics = m

	// Build the ancestor set for the children. Copy first; do
	// NOT mutate the caller's map.
	if len(n.Children) > 0 {
		next := make(map[repo.Hash]int64, len(ancestors)+len(own))
		for h, l := range ancestors {
			next[h] = l
		}
		for h, l := range own {
			next[h] = l
		}
		for _, c := range n.Children {
			analyzeNode(c, next)
		}
	}
}

// SummarizeChain rolls up per-node ChainMetrics for one root +
// its descendants into a ChainSummary.
func SummarizeChain(root *Node) ChainSummary {
	s := ChainSummary{RootBackupID: root.BackupID}
	walkChain(root, &s, 0)
	if s.UniqueChunkBytesSum > 0 {
		s.DedupRatioOverall = float64(s.LogicalBytesSum) / float64(s.UniqueChunkBytesSum)
	}
	return s
}

func walkChain(n *Node, s *ChainSummary, depth int) {
	s.NodeCount++
	s.LogicalBytesSum += n.LogicalBytes
	if n.Metrics != nil {
		s.UniqueChunkBytesSum += n.Metrics.UniqueChunkBytes
		s.SharedChunkBytesSum += n.Metrics.SharedWithAncestorsBytes
	}
	if depth+1 > s.MaxDepth {
		s.MaxDepth = depth + 1
	}
	if n.IsLeaf() {
		s.LeafCount++
	}
	for _, c := range n.Children {
		walkChain(c, s, depth+1)
	}
}
