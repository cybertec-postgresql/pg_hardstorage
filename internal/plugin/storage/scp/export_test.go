package scp

// ShellQuoteForTest exposes the unexported shellQuote helper to
// the _test package.  Single-quote escaping is the only
// shell-injection defence in this backend; testing it directly
// is worth the tiny export.
func ShellQuoteForTest(s string) string { return shellQuote(s) }

// The following expose the remote-command builders + prefix
// resolver so the _test package can assert quoting is applied
// exactly once (no double-quoting / injection) and that empty
// prefixes resolve for listing.
func ExistsCommandForTest(p string) string { return existsCommand(p) }
func StatCommandForTest(p string) string   { return statCommand(p) }
func ListCommandForTest(p string) string   { return listCommand(p) }

const StatNotFoundMarkerForTest = statNotFoundMarker

// ResolvePrefixForTest exercises resolvePrefix (List's path
// resolver) which — unlike resolve — must accept an empty prefix.
func ResolvePrefixForTest(root, prefix string) (string, error) {
	p := &Plugin{root: root}
	return p.resolvePrefix(prefix)
}

// ResolveForTest exercises the strict resolve used by the
// read/write paths, which must still refuse an empty key.
func ResolveForTest(root, key string) (string, error) {
	p := &Plugin{root: root}
	return p.resolve(key)
}
