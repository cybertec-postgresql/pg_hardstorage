//go:build mutation_chunkkey_no_suffix

package repo

// ChunkKey — MUTATED variant.  Drops the ".chk" suffix.  Selected
// by `go test -tags=mutation_chunkkey_no_suffix`.  Any test that
// round-trips a chunk through the storage layer must fail under
// this mutation: the writer will commit the object at one path,
// the reader will look at the same path (since it also computes
// the mutated form), but if anything ELSE in the codebase parses
// keys or reasons about the format, it'll diverge.  Specifically:
// `ParseChunkKey` rejects the suffix-less form and returns
// ErrNotAChunkKey, so cas_test.go's parse-round-trip tests fail.
//
// See internal/testkit/mutation/registry.go for the harness that
// runs `go test -tags=mutation_chunkkey_no_suffix` and asserts
// the suite fails.
func ChunkKey(hash Hash) string {
	s := hash.String()
	return "chunks/sha256/" + s[0:2] + "/" + s[2:4] + "/" + s
}
