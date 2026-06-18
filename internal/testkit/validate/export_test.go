package validate

// ExtractFieldForTest exposes the unexported extractField
// helper to the _test package.  Kept in a *_test.go file so
// it doesn't ship in the production binary.
func ExtractFieldForTest(s, prefix, suffix string) string {
	return extractField(s, prefix, suffix)
}

// IsRepoAlreadyExistsForTest exposes the unexported helper so
// the _test package can lock the idempotency contract: any
// of the three documented signals (exit-code 7, structured
// `conflict.repo_exists` code, or human "already exists at"
// substring) must return true.
func IsRepoAlreadyExistsForTest(err error, output []byte) bool {
	return isRepoAlreadyExists(err, output)
}

// CorruptIndexNameForTest exposes the unexported helper so the
// _test package can lock the torn_page un-wedge contract: a
// XX002 "is not a btree" error must yield the index name to
// REINDEX, and anything else must yield "".
func CorruptIndexNameForTest(err error) string {
	return corruptIndexName(err)
}
