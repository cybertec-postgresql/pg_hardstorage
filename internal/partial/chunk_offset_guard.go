//go:build !mutation_partial_offset_unchecked

package partial

// chunk_offset_guard.go — materialiseOneFile writes a table file's chunks
// in slice order and must confirm each chunk lands where its Offset says.
// A reordered or gapped chunk list that still sums to the file size would
// otherwise produce a byte-scrambled extraction that passes the size check
// (the same class as restore.materializeFile's bug #32; the partial mirror
// had none). Own file so the mutation registry can carry the always-true
// variant that reopens the silent-corruption hole.
func chunkOffsetContiguous(chunkOffset, writePosition int64) bool {
	return chunkOffset == writePosition
}
