// analysis.go — ChainMetrics: per-node chunk-overlap accounting across ancestor backups.
package chain

import (
	"context"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
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
// Memory: bounded. A node's distinct chunks go into a spill.HashSet (heap
// capped, the rest on disk) that lives only while its subtree is being
// analysed, so at any moment the sets held are those of the current
// root-to-node path — not one per backup, and not a Go map of billions of
// hashes.
//
// We do NOT walk orphans; their parent set is unknown so the
// metrics would be misleading.
func AnalyzeChain(g *Graph) { _ = AnalyzeChainContext(context.Background(), g) }

// AnalyzeChainContext is AnalyzeChain with cancellation and the error a
// manifest walk can hit. Metrics of nodes analysed before a failure stay
// populated.
func AnalyzeChainContext(ctx context.Context, g *Graph) error {
	for _, root := range g.Roots {
		if err := analyzeNode(ctx, root, nil, 0); err != nil {
			return err
		}
	}
	return nil
}

// analyzeNode is the per-node analyzer. ancestors holds the distinct-chunk
// set of every strict ancestor of n (root first). n's own set is built
// here, used for its metrics, appended for its children, and closed when
// its subtree is done. Siblings share the same ancestors slice contents
// but never each other's chunks (the append below is a fresh slice).
func analyzeNode(ctx context.Context, n *Node, ancestors []*spill.HashSet, ancestorDistinct int) error {
	own := spill.NewHashSet(spill.Options{})
	defer own.Close()
	// ancestorDistinct is the size of the UNION of the ancestors' sets,
	// carried down: a child adds exactly the chunks not already in them.
	m := &ChainMetrics{AncestorChunkCount: ancestorDistinct}
	if n.src != nil {
		err := n.src.EachFile(ctx, func(f *backup.FileEntry) error {
			for _, c := range f.Chunks {
				if !own.Add(c.Hash) {
					continue // first sight of a chunk within this manifest only
				}
				m.TotalChunks++
				m.TotalChunkBytes += c.Len
				shared := false
				for _, a := range ancestors {
					if a.Has(c.Hash) {
						shared = true
						break
					}
				}
				if shared {
					m.SharedWithAncestors++
					m.SharedWithAncestorsBytes += c.Len
				} else {
					m.UniqueChunks++
					m.UniqueChunkBytes += c.Len
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err := own.Err(); err != nil {
			return err
		}
	}
	if m.UniqueChunks > 0 {
		m.DedupRatioVsChain = float64(m.TotalChunks) / float64(m.UniqueChunks)
	}
	n.Metrics = m

	if len(n.Children) > 0 {
		next := make([]*spill.HashSet, len(ancestors)+1)
		copy(next, ancestors)
		next[len(ancestors)] = own
		for _, c := range n.Children {
			if err := analyzeNode(ctx, c, next, ancestorDistinct+m.UniqueChunks); err != nil {
				return err
			}
		}
	}
	return nil
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
