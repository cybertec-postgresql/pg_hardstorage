//go:build !mutation_materialize_offset_unchecked

package restore

// chunk_offset_guard.go — materializeFile writes chunks in slice
// order and must confirm each chunk lands where its Offset says. A
// reordered or gapped chunk list that still sums to the file size
// would otherwise restore byte-scrambled data that passes every other
// check (issue found by direct testing of materializeFile, which had
// none). Own file so the mutation registry can carry the always-true
// variant that reopens the silent-corruption hole.
func chunkOffsetContiguous(chunkOffset, writePosition int64) bool {
	return chunkOffset == writePosition
}
