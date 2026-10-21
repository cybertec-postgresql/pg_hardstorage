// compare.go — pairwise backup comparison: file-level diff + chunk-level dedup overlap.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
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
// Memory: O(F_a + F_b) for the per-file index (path, size, a digest of
// the chunk list) plus a bounded-memory set per side for the chunk-level
// diff. File counts are small even for enormous databases (a relation
// segment is 1 GiB, so ~200K files per 200 TB), whereas chunk counts are
// billions — which is why only the chunk side spills to disk. Each side
// is walked once through EachFile, so streamed manifests work.
// Determinism: TopFileDeltas sort is (|delta| desc, path asc) so ties
// are stable across runs.
func Compare(a, b *Manifest, opts CompareOptions) (*ComparisonResult, error) {
	return CompareContext(context.Background(), a, b, opts)
}

// CompareContext is Compare with a context for the manifest walks.
func CompareContext(ctx context.Context, a, b *Manifest, opts CompareOptions) (*ComparisonResult, error) {
	if a == nil || b == nil {
		return nil, errors.New("backup: Compare requires both manifests to be non-nil")
	}
	if opts.TopN <= 0 {
		opts.TopN = DefaultCompareTopN
	}

	setA := spill.NewHashSet(spill.Options{})
	defer setA.Close()
	setB := spill.NewHashSet(spill.Options{})
	defer setB.Close()

	aIdx, aSide, err := scanSide(ctx, a, setA, nil)
	if err != nil {
		return nil, err
	}
	res := &ComparisonResult{A: aSide.ComparisonSide}
	bIdx, bSide, err := scanSide(ctx, b, setB, setA)
	if err != nil {
		return nil, err
	}
	res.B = bSide.ComparisonSide

	// Chunk-level diff. B's scan counted the chunks shared with A; the
	// rest of each side is exclusive to it.
	res.ChunkCounts.Shared = bSide.shared
	res.ChunkCounts.SharedBytes = bSide.sharedBytes
	res.ChunkCounts.BOnly = res.B.UniqueChunkCount - bSide.shared
	res.ChunkCounts.BOnlyBytes = res.B.UniqueChunkBytes - bSide.sharedBytes
	res.ChunkCounts.AOnly = res.A.UniqueChunkCount - bSide.shared
	res.ChunkCounts.AOnlyBytes = res.A.UniqueChunkBytes - bSide.sharedBytes

	// File-level diff.
	allPaths := make([]string, 0, len(aIdx)+len(bIdx))
	for p := range aIdx {
		allPaths = append(allPaths, p)
	}
	for p := range bIdx {
		if _, ok := aIdx[p]; !ok {
			allPaths = append(allPaths, p)
		}
	}
	sort.Strings(allPaths)

	deltas := make([]FileDelta, 0, len(allPaths))
	var sharedLogical int64
	for _, p := range allPaths {
		af, aOK := aIdx[p]
		bf, bOK := bIdx[p]
		switch {
		case aOK && !bOK:
			res.FileCounts.OnlyInA++
			deltas = append(deltas, FileDelta{
				Path: p, ASize: af.size, BSize: 0,
				Delta: -af.size, Class: "removed",
			})
		case !aOK && bOK:
			res.FileCounts.OnlyInB++
			deltas = append(deltas, FileDelta{
				Path: p, ASize: 0, BSize: bf.size,
				Delta: bf.size, Class: "added",
			})
		default:
			res.FileCounts.InBoth++
			if af != bf {
				res.FileCounts.Changed++
				deltas = append(deltas, FileDelta{
					Path: p, ASize: af.size, BSize: bf.size,
					Delta: bf.size - af.size, Class: "changed",
				})
			} else {
				// Same chunk list + same size = same bytes.
				// Count toward shared-logical-bytes.
				sharedLogical += af.size
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

	// Logical bytes.
	res.LogicalBytes = LogicalBytesBreakdown{
		A:      res.A.LogicalBytes,
		B:      res.B.LogicalBytes,
		Delta:  res.B.LogicalBytes - res.A.LogicalBytes,
		Shared: sharedLogical,
	}

	return res, nil
}

// fileSig identifies a file's bytes: its size and a digest of the
// ordered chunk list (hash, length, offset). Equal sigs mean the same
// bytes; Mode and ModTime are deliberately not part of it (touching a
// file's mtime does not change its bytes).
type fileSig struct {
	size   int64
	digest [32]byte
}

type sideScan struct {
	ComparisonSide
	shared      int
	sharedBytes int64
}

// scanSide walks one manifest once: builds its per-file index and its
// rollup, adds its chunks to set, and — when other is the other side's
// completed set — counts the chunks the two share.
func scanSide(ctx context.Context, m *Manifest, set, other *spill.HashSet) (map[string]fileSig, sideScan, error) {
	idx := make(map[string]fileSig)
	sc := sideScan{ComparisonSide: ComparisonSide{
		BackupID:  m.BackupID,
		Type:      string(m.Type),
		FileCount: m.NumFiles(),
	}}
	if !m.StoppedAt.IsZero() {
		sc.StoppedAt = m.StoppedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	var buf [8]byte
	err := m.EachFile(ctx, func(f *FileEntry) error {
		sc.LogicalBytes += f.Size
		h := sha256.New()
		for _, c := range f.Chunks {
			h.Write(c.Hash[:])
			binary.BigEndian.PutUint64(buf[:], uint64(c.Len))
			h.Write(buf[:])
			binary.BigEndian.PutUint64(buf[:], uint64(c.Offset))
			h.Write(buf[:])
			if set.Add(c.Hash) {
				sc.UniqueChunkCount++
				sc.UniqueChunkBytes += c.Len
				if other != nil && other.Has(c.Hash) {
					sc.shared++
					sc.sharedBytes += c.Len
				}
			}
		}
		var sig fileSig
		sig.size = f.Size
		copy(sig.digest[:], h.Sum(nil))
		idx[f.Path] = sig
		return nil
	})
	if err == nil {
		err = set.Err()
	}
	return idx, sc, err
}

// abs returns |x| for int64 without overflow concerns at the
// values we see (file sizes fit comfortably).
func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
