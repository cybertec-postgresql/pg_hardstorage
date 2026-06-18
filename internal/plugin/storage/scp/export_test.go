package scp

// ShellQuoteForTest exposes the unexported shellQuote helper to
// the _test package.  Single-quote escaping is the only
// shell-injection defence in this backend; testing it directly
// is worth the tiny export.
func ShellQuoteForTest(s string) string { return shellQuote(s) }
