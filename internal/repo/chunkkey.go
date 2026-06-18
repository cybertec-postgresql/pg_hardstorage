//go:build !mutation_chunkkey_no_suffix

package repo

// ChunkKey returns the canonical on-disk / on-S3 key for a chunk hash.
//
// Format: chunks/sha256/aa/bb/aabb<remaining-60-hex>.chk
//
// The 2+2+60 split provides ~65k * 65k unique sub-prefixes which
// keeps per-directory size bounded at filesystem scale and gives
// object stores a useful LIST partitioning key.
//
// Mutation-testing note: a deliberately-broken variant lives in
// chunkkey_mutation_chunkkey_no_suffix.go and is selected by
// `go test -tags=mutation_chunkkey_no_suffix`.  The testkit's
// mutation harness uses that tag to confirm the test suite
// catches the regression.  See internal/testkit/mutation.
func ChunkKey(hash Hash) string {
	s := hash.String()
	return "chunks/sha256/" + s[0:2] + "/" + s[2:4] + "/" + s + ".chk"
}
