package jira

import (
	"testing"
)

// jiraEscape is the unexported defense-in-depth helper that escapes
// strings for safe inclusion inside JQL double-quoted literals.
// Tested as an internal unit so the escape behaviour is pinned even
// if the call sites change.
func TestJiraEscape_PreservesNormalText(t *testing.T) {
	for _, in := range []string{
		"db1",
		"backup.manifest.replica_failed",
		"deployment=db1 backup=full.20260427",
		"GDPR Art 17 #4421",
		"OPS-1234",
		"",
	} {
		got := jiraEscape(in)
		if got != in {
			t.Errorf("jiraEscape(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestJiraEscape_EscapesQuoteAndBackslash(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`foo"bar`, `foo\"bar`},
		{`foo\bar`, `foo\\bar`},
		// Both: backslash MUST be doubled FIRST so the `\"` produced
		// by quote-escaping isn't itself doubled.
		{`a"b\c`, `a\"b\\c`},
		{`\"`, `\\\"`},
		// Reviewer's classic injection vector — must not let the
		// attacker break out of the JQL literal.
		{`foo" OR 1=1 --`, `foo\" OR 1=1 --`},
		// Empty + only-special.
		{``, ``},
		{`""`, `\"\"`},
		{`\\`, `\\\\`},
	}
	for _, c := range cases {
		if got := jiraEscape(c.in); got != c.want {
			t.Errorf("jiraEscape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// After escape, the result MUST NOT contain any unescaped " — every
// quote must be preceded by a backslash. This is the
// machine-checkable invariant for "you can't break out of the
// JQL literal."
func TestJiraEscape_NoUnescapedQuotes(t *testing.T) {
	for _, in := range []string{
		`"`,
		`abc"def`,
		`""""`,
		`a\"b`,
		`he said "hi"`,
	} {
		got := jiraEscape(in)
		// Scan: every " must have a preceding \.
		for i := 0; i < len(got); i++ {
			if got[i] != '"' {
				continue
			}
			// Count preceding backslashes; if odd, the quote is
			// escaped. If even (including zero), it's unescaped.
			backslashes := 0
			for j := i - 1; j >= 0 && got[j] == '\\'; j-- {
				backslashes++
			}
			if backslashes%2 == 0 {
				t.Errorf("unescaped \" at position %d in jiraEscape(%q) = %q",
					i, in, got)
			}
		}
		// Also: the result must never end with an odd number of
		// trailing backslashes (would escape the closing JQL quote).
		trailing := 0
		for i := len(got) - 1; i >= 0 && got[i] == '\\'; i-- {
			trailing++
		}
		if trailing%2 != 0 {
			t.Errorf("trailing odd-backslash count %d in jiraEscape(%q) = %q",
				trailing, in, got)
		}
	}
}
