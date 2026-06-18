// compare.go — pairwise backup comparison: file-level diff + chunk-level dedup overlap.
package backup

import (
	"errors"
	"sort"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// ComparisonResult is the structured output of Compare. It
// answers two operationally-distinct questions side-by-side:
//
//  1. WHAT FILES CHANGED? — the per-file diff (added / removed
//     / changed) gives the operator a "what data moved between
//     these two backups?" view. Useful for incremental-size
//     forensics, audit ("was this table really truncated?"),
//     and dedup-bloat investigations.
//
//  2. HOW MUCH DATA WAS DEDUPED? — the per-chunk overlap gives
//     the operator a "what fraction of B was already in A?"
//     view. The shared-chunk byte count is what an incremental
//     backup naturally avoids re-uploading; the B-only chunk
//     count is the new bytes B genuinely contributed.
//
// Both views come from a single pass over each manifest's files
// + chunk slices.
type ComparisonResult struct {
	A ComparisonSide `json:"a"`
	B ComparisonSide `json:"b"`

	// FileCounts is the per-class file accounting. Sums to
	// max(A.FileCount, B.FileCount).
	FileCounts FileCountsBreakdown `json:"file_counts"`

	// ChunkCounts is the per-class chunk accounting using
	// HASH-DEDUPED unique chunks per side. Shared+AOnly
	// equals A's UniqueChunkCount; Shared+BOnly equals B's
	// UniqueChunkCount.
	ChunkCounts ChunkCountsBreakdown `json:"chunk_counts"`

	// LogicalBytes summarises plaintext file-size totals.
	// "shared" is the sum of byte-overlap on files that
	// exist in both manifests with the same chunk set; in
	// practice this is a useful approximation for "data
	// physically unchanged between A and B."
	LogicalBytes LogicalBytesBreakdown `json:"logical_bytes"`

	// TopFileDeltas surfaces the largest changes by absolute
	// byte delta (added or removed). Capped to TopN entries
	// for readable output; a v1.x flag may expose the full
	// list for forensic dumps.
	TopFileDeltas []FileDelta `json:"top_file_deltas,omitempty"`
}

// ComparisonSide is the per-manifest summary embedded in the
// result. Same shape on both sides.
type ComparisonSide struct {
	BackupID         string `json:"backup_id"`
	Type             string `json:"type"`
	StoppedAt        string `json:"stopped_at"`
	FileCount        int    `json:"file_count"`
	UniqueChunkCount int    `json:"unique_chunk_count"`
	UniqueChunkBytes int64  `json:"unique_chunk_bytes"`
	LogicalBytes     int64  `json:"logical_bytes"`
}

// FileCountsBreakdown counts files by inclusion class.
type FileCountsBreakdown struct {
	OnlyInA int `json:"only_in_a"`
	OnlyInB int `json:"only_in_b"`
	InBoth  int `json:"in_both"`
	// Changed counts files present in BOTH manifests with
	// differing chunk sets (size, mtime, or chunk-hash list
	// differs). A subset of InBoth.
	Changed int `json:"changed"`
}

// ChunkCountsBreakdown counts hash-unique chunks. Shared is
// chunks that appear in BOTH manifests' chunk sets; AOnly
// (resp. BOnly) is chunks present only in A (resp. B).
type ChunkCountsBreakdown struct {
	Shared      int   `json:"shared"`
	AOnly       int   `json:"a_only"`
	BOnly       int   `json:"b_only"`
	SharedBytes int64 `json:"shared_bytes"`
	AOnlyBytes  int64 `json:"a_only_bytes"`
	BOnlyBytes  int64 `json:"b_only_bytes"`
}

// LogicalBytesBreakdown summarises plaintext file-byte totals.
// "Shared bytes" is the sum of same-bytes files (same path +
// chunk set on both sides); it's a useful approximation for
// "data unchanged between A and B" without doing per-byte
// diffing.
type LogicalBytesBreakdown struct {
	A      int64 `json:"a"`
	B      int64 `json:"b"`
	Delta  int64 `json:"delta"` // B - A; positive = grew
	Shared int64 `json:"shared"`
}

// FileDelta is one entry in TopFileDeltas. The path is keyed
// at the file level; ASize is the file's bytes in A (0 if
// only-in-B); BSize is the file's bytes in B (0 if only-in-A);
// Delta is BSize - ASize. Class is "added", "removed", or
// "changed".
type FileDelta struct {
	Path  string `json:"path"`
	ASize int64  `json:"a_size"`
	BSize int64  `json:"b_size"`
	Delta int64  `json:"delta"`
	Class string `json:"class"`
}

// CompareOptions tunes the comparison output.
type CompareOptions struct {
	// TopN caps the size of TopFileDeltas. Zero means "use
	// the default" (DefaultCompareTopN).
	TopN int
}

// DefaultCompareTopN is the default cap on TopFileDeltas.
const DefaultCompareTopN = 20

// Compare diffs two manifests and returns a structured
// ComparisonResult. Both manifests must be non-nil; an empty
// FileEntry slice on either side is treated as "no files",
// not an error.
//
// CPU + memory: O(F_a + F_b + C_a + C_b) where F is file count
// and C is chunk count per side. Two-pass: build per-side
// indices, then walk the union of paths and the union of
// chunk hashes. Determinism: TopFileDeltas sort is
// (|delta| desc, path asc) so ties are stable across runs.
func Compare(a, b *Manifest, opts CompareOptions) (*ComparisonResult, error) {
	if a == nil || b == nil {
		return nil, errors.New("backup: Compare requires both manifests to be non-nil")
	}
	if opts.TopN <= 0 {
		opts.TopN = DefaultCompareTopN
	}

	res := &ComparisonResult{
		A: summarizeManifestForCompare(a),
		B: summarizeManifestForCompare(b),
	}

	// File-level diff.
	aByPath := indexFilesByPath(a.Files)
	bByPath := indexFilesByPath(b.Files)
	allPaths := make([]string, 0, len(aByPath)+len(bByPath))
	for p := range aByPath {
		allPaths = append(allPaths, p)
	}
	for p := range bByPath {
		if _, ok := aByPath[p]; !ok {
			allPaths = append(allPaths, p)
		}
	}
	sort.Strings(allPaths)

	deltas := make([]FileDelta, 0, len(allPaths))
	var sharedLogical int64
	for _, p := range allPaths {
		af, aOK := aByPath[p]
		bf, bOK := bByPath[p]
		switch {
		case aOK && !bOK:
			res.FileCounts.OnlyInA++
			deltas = append(deltas, FileDelta{
				Path: p, ASize: af.Size, BSize: 0,
				Delta: -af.Size, Class: "removed",
			})
		case !aOK && bOK:
			res.FileCounts.OnlyInB++
			deltas = append(deltas, FileDelta{
				Path: p, ASize: 0, BSize: bf.Size,
				Delta: bf.Size, Class: "added",
			})
		default:
			res.FileCounts.InBoth++
			if !filesEqual(af, bf) {
				res.FileCounts.Changed++
				deltas = append(deltas, FileDelta{
					Path: p, ASize: af.Size, BSize: bf.Size,
					Delta: bf.Size - af.Size, Class: "changed",
				})
			} else {
				// Same chunk set + same size = same bytes.
				// Count toward shared-logical-bytes.
				sharedLogical += af.Size
			}
		}
	}
	// TopFileDeltas: sort by |delta| desc, then path asc.
	sort.Slice(deltas, func(i, j int) bool {
		ai, aj := abs(deltas[i].Delta), abs(deltas[j].Delta)
		if ai != aj {
			return ai > aj
		}
		return deltas[i].Path < deltas[j].Path
	})
	if len(deltas) > opts.TopN {
		deltas = deltas[:opts.TopN]
	}
	res.TopFileDeltas = deltas

	// Chunk-level diff using the per-side hash→size maps.
	aChunks := uniqueChunkSizes(a.Files)
	bChunks := uniqueChunkSizes(b.Files)
	for h, sz := range aChunks {
		if bSz, ok := bChunks[h]; ok {
			res.ChunkCounts.Shared++
			// Shared bytes use the A-side size; for the
			// CAS-deduped case the sizes match.
			res.ChunkCounts.SharedBytes += sz
			_ = bSz
		} else {
			res.ChunkCounts.AOnly++
			res.ChunkCounts.AOnlyBytes += sz
		}
	}
	for h, sz := range bChunks {
		if _, ok := aChunks[h]; !ok {
			res.ChunkCounts.BOnly++
			res.ChunkCounts.BOnlyBytes += sz
		}
	}

	// Logical bytes.
	res.LogicalBytes = LogicalBytesBreakdown{
		A:      res.A.LogicalBytes,
		B:      res.B.LogicalBytes,
		Delta:  res.B.LogicalBytes - res.A.LogicalBytes,
		Shared: sharedLogical,
	}

	return res, nil
}

// summarizeManifestForCompare builds the per-side rollup
// embedded in ComparisonResult. Mirrors the
// per-row summary the list / show commands compute.
func summarizeManifestForCompare(m *Manifest) ComparisonSide {
	uniq := uniqueChunkSizes(m.Files)
	var uniqBytes int64
	for _, sz := range uniq {
		uniqBytes += sz
	}
	var logical int64
	for _, f := range m.Files {
		logical += f.Size
	}
	stoppedAt := ""
	if !m.StoppedAt.IsZero() {
		stoppedAt = m.StoppedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return ComparisonSide{
		BackupID:         m.BackupID,
		Type:             string(m.Type),
		StoppedAt:        stoppedAt,
		FileCount:        len(m.Files),
		UniqueChunkCount: len(uniq),
		UniqueChunkBytes: uniqBytes,
		LogicalBytes:     logical,
	}
}

// indexFilesByPath builds a path→entry index over a manifest's
// Files slice. A FileEntry's Path is unique within a manifest;
// duplicates are silently last-wins (the manifest store would
// have refused duplicate keys at commit time).
func indexFilesByPath(files []FileEntry) map[string]FileEntry {
	out := make(map[string]FileEntry, len(files))
	for _, f := range files {
		out[f.Path] = f
	}
	return out
}

// uniqueChunkSizes builds a hash→len map across every
// FileEntry's Chunks slice. CAS dedup means the same hash
// always has the same Len; pick whichever wins on insertion
// (lengths are identical so it's idempotent).
func uniqueChunkSizes(files []FileEntry) map[repo.Hash]int64 {
	out := make(map[repo.Hash]int64)
	for _, f := range files {
		for _, c := range f.Chunks {
			out[c.Hash] = c.Len
		}
	}
	return out
}

// filesEqual reports whether two FileEntry values represent
// the same bytes. Same Size + same chunk-hash list (in
// order). Doesn't compare Mode or ModTime — those can change
// without the bytes changing (touch a file's mtime; PG-side
// processes do this).
func filesEqual(a, b FileEntry) bool {
	if a.Size != b.Size {
		return false
	}
	if len(a.Chunks) != len(b.Chunks) {
		return false
	}
	for i := range a.Chunks {
		if a.Chunks[i].Hash != b.Chunks[i].Hash {
			return false
		}
		if a.Chunks[i].Len != b.Chunks[i].Len {
			return false
		}
		if a.Chunks[i].Offset != b.Chunks[i].Offset {
			return false
		}
	}
	return true
}

// abs returns |x| for int64 without overflow concerns at the
// values we see (file sizes fit comfortably).
func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
