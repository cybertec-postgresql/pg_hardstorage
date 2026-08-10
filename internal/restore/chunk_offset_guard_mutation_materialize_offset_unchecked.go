//go:build mutation_materialize_offset_unchecked

package restore

// chunkOffsetContiguous — MUTATED: the offset check is a no-op,
// restoring the pre-fix world where materializeFile writes chunks in
// slice order without confirming their offsets, so a reordered chunk
// list silently produces a byte-scrambled restored file.
func chunkOffsetContiguous(chunkOffset, writePosition int64) bool { return true }
