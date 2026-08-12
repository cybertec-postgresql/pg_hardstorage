//go:build mutation_partial_offset_unchecked

package partial

// chunkOffsetContiguous — MUTATED: the offset check is a no-op, restoring
// the pre-fix world where materialiseOneFile writes chunks in slice order
// without confirming their offsets, so a reordered chunk list silently
// produces a byte-scrambled extracted table file.
func chunkOffsetContiguous(chunkOffset, writePosition int64) bool { return true }
