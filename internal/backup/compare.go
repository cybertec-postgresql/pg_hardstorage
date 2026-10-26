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

// compareSortMemBytes is the in-memory buffer of each side's per-file
// signature sorter (0 = the sorter's default). A variable so tests can force
// the spill path with a handful of files.
var compareSortMemBytes int

// Compare diffs two manifests and returns a structured
// ComparisonResult. Both manifests must be non-nil; an empty
// FileEntry slice on either side is treated as "no files",
// not an error.
//
// Memory: bounded, independent of both file and chunk counts. Chunks go
// through a bounded-memory spill set per side; the per-file signatures
// (path, size, a digest of the chunk list) go through an external sorter
// per side and are merge-joined in path order, so a manifest with 100M
// files costs a few tens of MiB of heap rather than tens of GiB. Each side
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

	// Per-file signatures go to external sorters keyed by path rather than
	// into a map: a manifest with 100M files would otherwise cost tens of
	// GiB of heap per side. The two sorted streams are merge-joined below.
	aIdx := spill.NewSorter(spill.SorterOptions{MemBytes: compareSortMemBytes})
	defer aIdx.Close()
	bIdx := spill.NewSorter(spill.SorterOptions{MemBytes: compareSortMemBytes})
	defer bIdx.Close()

	aSide, err := scanSide(ctx, a, setA, nil, aIdx)
	if err != nil {
		return nil, err
	}
	res := &ComparisonResult{A: aSide.ComparisonSide}
	bSide, err := scanSide(ctx, b, setB, setA, bIdx)
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

	// File-level diff. Only the TopN ranked deltas are kept (a bounded
	// insertion list), not one FileDelta per differing file: a diff of
	// two multi-million-file backups would otherwise materialise them
	// all plus a sorted copy of every path.
	var sharedLogical int64
	top := make([]FileDelta, 0, opts.TopN)
	// before reports whether x ranks ahead of y: |delta| desc, path asc.
	before := func(x, y FileDelta) bool {
		ax, ay := abs(x.Delta), abs(y.Delta)
		if ax != ay {
			return ax > ay
		}
		return x.Path < y.Path
	}
	offer := func(d FileDelta) {
		if len(top) == opts.TopN && !before(d, top[len(top)-1]) {
			return
		}
		i := sort.Search(len(top), func(i int) bool { return before(d, top[i]) })
		if len(top) < opts.TopN {
			top = append(top, FileDelta{})
		}
		copy(top[i+1:], top[i:])
		top[i] = d
	}
	ai, err := aIdx.Iter()
	if err != nil {
		return nil, err
	}
	defer ai.Close()
	bi, err := bIdx.Iter()
	if err != nil {
		return nil, err
	}
	defer bi.Close()
	// pull copies the record out of the iterator's reusable buffers.
	type rec struct {
		ok   bool
		path string
		sig  fileSig
	}
	pull := func(it *spill.Iter) (rec, error) {
		k, v, ok, err := it.Next()
		if err != nil || !ok {
			return rec{}, err
		}
		r := rec{ok: true, path: string(k)}
		r.sig.size = int64(binary.BigEndian.Uint64(v[:8]))
		copy(r.sig.digest[:], v[8:])
		return r, nil
	}
	ar, err := pull(ai)
	if err != nil {
		return nil, err
	}
	br, err := pull(bi)
	if err != nil {
		return nil, err
	}
	for ar.ok || br.ok {
		switch {
		case !br.ok || (ar.ok && ar.path < br.path):
			res.FileCounts.OnlyInA++
			offer(FileDelta{Path: ar.path, ASize: ar.sig.size, BSize: 0, Delta: -ar.sig.size, Class: "removed"})
			ar, err = pull(ai)
		case !ar.ok || br.path < ar.path:
			res.FileCounts.OnlyInB++
			offer(FileDelta{Path: br.path, ASize: 0, BSize: br.sig.size, Delta: br.sig.size, Class: "added"})
			br, err = pull(bi)
		default:
			res.FileCounts.InBoth++
			if ar.sig != br.sig {
				res.FileCounts.Changed++
				offer(FileDelta{Path: ar.path, ASize: ar.sig.size, BSize: br.sig.size, Delta: br.sig.size - ar.sig.size, Class: "changed"})
			} else {
				// Same chunk list + same size = same bytes.
				sharedLogical += ar.sig.size
			}
			if ar, err = pull(ai); err == nil {
				br, err = pull(bi)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	res.TopFileDeltas = top

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
func scanSide(ctx context.Context, m *Manifest, set, other *spill.HashSet, idx *spill.Sorter) (sideScan, error) {
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
		var val [8 + 32]byte
		binary.BigEndian.PutUint64(val[:8], uint64(sig.size))
		copy(val[8:], sig.digest[:])
		return idx.Add([]byte(f.Path), val[:])
	})
	if err == nil {
		err = set.Err()
	}
	return sc, err
}

// abs returns |x| for int64 without overflow concerns at the
// values we see (file sizes fit comfortably).
func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
