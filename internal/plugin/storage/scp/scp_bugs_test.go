package scp_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/scp"
)

// Bug 5: exists() previously wrapped an already-shellQuoted path in a
// second `sh -c '...'`, so the quoting cancelled out. The command
// builder must quote the path exactly once and never nest it in an
// inner shell — otherwise $()/backticks in a key would execute and
// spaces would break the check.
func TestExistsCommand_QuotesExactlyOnce(t *testing.T) {
	// A path with a space and a command-substitution payload: if the
	// path is quoted exactly once (single quotes), the whole thing is
	// inert; a second `sh -c '...'` layer would strip the quoting.
	full := "/srv/repo/a b/$(touch pwned)"
	cmd := scp.ExistsCommandForTest(full)

	if strings.Contains(cmd, "sh -c") {
		t.Fatalf("exists command must not nest an inner shell: %q", cmd)
	}
	// The path must appear inside exactly one single-quoted literal.
	want := scp.ShellQuoteForTest(full)
	if !strings.Contains(cmd, want) {
		t.Fatalf("path not single-quoted once: cmd=%q want substring %q", cmd, want)
	}
	// The command-substitution must be inside the single quotes (inert),
	// never sitting outside where the shell would expand it.
	if strings.Count(cmd, "'") != strings.Count(want, "'") {
		t.Fatalf("unexpected extra quotes (double-quoting?): cmd=%q", cmd)
	}
	if idx := strings.Index(cmd, "$(touch pwned)"); idx < 0 {
		t.Fatalf("payload missing from command: %q", cmd)
	} else {
		// The '$(' must be preceded (somewhere before it) by the
		// opening quote of the literal and never be un-quoted.
		if !strings.HasPrefix(cmd, "[ -e '") {
			t.Fatalf("path is not opened with a single quote: %q", cmd)
		}
	}
}

// Bug 28: statCommand must emit a distinct not-found marker (printed
// on a clean exit) so Stat can tell "file absent" from a transport
// failure. Verify the marker is present and the path is quoted once.
func TestStatCommand_HasNotFoundMarker(t *testing.T) {
	cmd := scp.StatCommandForTest("/srv/repo/x y")
	if !strings.Contains(cmd, scp.StatNotFoundMarkerForTest) {
		t.Fatalf("stat command must emit the not-found marker: %q", cmd)
	}
	if strings.Contains(cmd, "2>/dev/null") {
		t.Fatalf("stat command must not swallow stderr (transport errors must propagate): %q", cmd)
	}
	if !strings.Contains(cmd, scp.ShellQuoteForTest("/srv/repo/x y")) {
		t.Fatalf("path not single-quoted: %q", cmd)
	}
}

// Bug 30: listCommand must NOT redirect find's stderr to /dev/null
// (real errors would be swallowed) and must guard the absent-root
// case explicitly so a plain find exit-1 is no longer conflated with
// "empty".
func TestListCommand_DoesNotSwallowErrors(t *testing.T) {
	cmd := scp.ListCommandForTest("/srv/repo")
	if strings.Contains(cmd, "2>/dev/null") {
		t.Fatalf("list command must not swallow find's stderr: %q", cmd)
	}
	if !strings.Contains(cmd, "find ") {
		t.Fatalf("list command must run find: %q", cmd)
	}
	// Absent-root guard so an empty listing is unambiguous (exit 0),
	// distinct from a find failure.
	if !strings.Contains(cmd, "! -e") {
		t.Fatalf("list command must guard the absent-root case: %q", cmd)
	}
}

// Bug 29: List(prefix="") must work (repo.Wipe calls List(ctx, "")).
// resolvePrefix accepts the empty prefix (=> repo root); the strict
// resolve used by read/write paths still refuses it.
func TestResolvePrefix_AllowsEmpty(t *testing.T) {
	got, err := scp.ResolvePrefixForTest("/srv/repo", "")
	if err != nil {
		t.Fatalf("resolvePrefix(\"\") must succeed for listing; got %v", err)
	}
	if got != "/srv/repo" {
		t.Fatalf("resolvePrefix(\"\") = %q, want repo root %q", got, "/srv/repo")
	}
	// A non-empty prefix still joins under the root.
	got, err = scp.ResolvePrefixForTest("/srv/repo", "chunks/aa")
	if err != nil {
		t.Fatalf("resolvePrefix(non-empty): %v", err)
	}
	if got != "/srv/repo/chunks/aa" {
		t.Fatalf("resolvePrefix = %q, want %q", got, "/srv/repo/chunks/aa")
	}
	// Traversal is still refused.
	if _, err := scp.ResolvePrefixForTest("/srv/repo", "../etc"); err == nil {
		t.Fatal("resolvePrefix must still refuse '..'")
	}
}

// The strict resolve (Put/Get/Stat path) must keep refusing empty
// keys — only listing is allowed to pass "".
func TestResolve_StillRefusesEmpty(t *testing.T) {
	if _, err := scp.ResolveForTest("/srv/repo", ""); err == nil {
		t.Fatal("resolve(\"\") must still be refused on the read/write path")
	}
}
