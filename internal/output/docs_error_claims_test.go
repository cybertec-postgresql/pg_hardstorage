package output_test

// docs_error_claims_test.go — the error codes and exit codes the prose
// promises must be the ones the binary produces.
//
// Two guards already existed either side of docs/reference/:
// errorcode_truthfulness_test.go walks code→doc at NAMESPACE level,
// and exitcode_truthfulness_test.go pins the reference page's mapping
// table against the dispatcher. Neither looks at the other 100-odd
// pages, and both directions leaked there:
//
//   - `storage.no_space` and `kms.key_missing` were documented in
//     troubleshooting.md with "(exit 8)". Neither code is emitted
//     anywhere in the tree, and even if it were, only the `unreachable`
//     leaf of those namespaces routes to 8 — the reference page says so
//     explicitly. An operator following that page writes
//     `if [ $? -eq 8 ]` around a disk-full case that exits 1.
//   - `usage.no_pg_verifybackup` (exit 2) in operator-guide.md is
//     really `verify.missing_tool` (exit 9). Misuse and
//     verification-failed are opposite ends of a cron policy: one says
//     "your command was wrong", the other "your backup may be bad".
//
// The namespace-level guard could not see any of these, because
// `storage`, `kms` and `usage` are all real namespaces. That is the
// same shape as the jq-path hole: the prefix is right, the leaf is
// fiction.
//
// Both guards below key off the namespaces that production code
// actually emits, so a dotted token in the docs is only ever judged
// when it is plausibly one of our codes — `pg_hardstorage.yaml` and
// `storage.StoragePlugin` are not error codes and are not treated as
// any.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// docsClaimExemptPrefixes are pages describing past releases: a
// changelog entry naming a code that has since been renamed is a
// historical record, not a claim about this binary.
var docsClaimExemptPrefixes = []string{
	"docs/changelog.md",
	"docs/release-notes/",
}

var (
	// `ns.leaf` in backticks.
	docCodeRe = regexp.MustCompile("`([a-z_][a-z0-9_]*)\\.([a-z0-9_][a-z0-9_.]*)`")
	// `ns.leaf` … exit N, within one line and a short window, so a
	// code and an unrelated exit number in the same sentence are not
	// paired up.
	docExitClaimRe = regexp.MustCompile(
		"`([a-z_][a-z0-9_]*\\.[a-z0-9_.]+)`[^`\\n]{0,40}?\\bexits?\\s+(?:with\\s+)?(?:code\\s+)?(\\d+)")
)

// emittedNamespaces reduces the emitted code set to its namespaces.
func emittedNamespaces(codes map[string]string) map[string]bool {
	ns := map[string]bool{}
	for code := range codes {
		if i := strings.Index(code, "."); i > 0 {
			ns[code[:i]] = true
		}
	}
	return ns
}

type docClaim struct {
	code  string
	exit  int
	where string
}

// scanDocClaims walks the docs once and returns both the dotted codes
// named and the (code, exit) pairs claimed.
func scanDocClaims(t *testing.T, root string) (codes []docClaim, exits []docClaim) {
	t.Helper()
	files := 0
	walkDocFiles(t, root, func(rel string, lines []string) {
		for _, pfx := range docsClaimExemptPrefixes {
			if strings.HasPrefix(rel, pfx) {
				return
			}
		}
		files++
		for i, line := range lines {
			where := rel + ":" + strconv.Itoa(i+1)
			for _, m := range docCodeRe.FindAllStringSubmatch(line, -1) {
				codes = append(codes, docClaim{code: m[1] + "." + m[2], where: where})
			}
			for _, m := range docExitClaimRe.FindAllStringSubmatch(line, -1) {
				n, err := strconv.Atoi(m[2])
				if err != nil {
					continue
				}
				exits = append(exits, docClaim{code: m[1], exit: n, where: where})
			}
		}
	})
	if files == 0 {
		t.Fatal("read no documentation pages — every assertion below would hold vacuously")
	}
	return codes, exits
}

// TestDocumentedExitClaimsMatchTheDispatcher is the guard that would
// have caught "(exit 8)" on a code that exits 1.
func TestDocumentedExitClaimsMatchTheDispatcher(t *testing.T) {
	root := repoRootFromOutput(t)
	emitted := emittedCodes(t, root)
	if len(emitted) < 100 {
		t.Fatalf("found only %d error codes in the tree — the AST scan stopped matching, so "+
			"the namespace filter below is meaningless", len(emitted))
	}
	ns := emittedNamespaces(emitted)
	_, claims := scanDocClaims(t, root)

	checked := 0
	var bad []string
	seen := map[string]bool{}
	for _, c := range claims {
		prefix, _, _ := strings.Cut(c.code, ".")
		if !ns[prefix] {
			continue // not one of our codes
		}
		checked++
		got := int(output.ExitCodeFor(output.NewError(c.code, "x")))
		if got == c.exit {
			continue
		}
		key := c.code + "\x00" + strconv.Itoa(c.exit)
		if seen[key] {
			continue
		}
		seen[key] = true
		bad = append(bad, c.where+"\n      `"+c.code+"` is documented as exit "+
			strconv.Itoa(c.exit)+", but ExitCodeFor routes it to "+strconv.Itoa(got))
	}

	if checked == 0 {
		t.Fatal("matched no (code, exit) claims in the docs — the regex stopped matching and " +
			"this test now asserts nothing")
	}
	t.Logf("checked %d documented (code, exit) claim(s)", checked)

	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("%d documented exit-code claim(s) disagree with the dispatcher:\n  %s\n\n"+
			"Exit codes are the scriptable half of the contract — an operator wires "+
			"`if [ $? -eq N ]` straight off these pages, and a wrong N is a branch that "+
			"never fires. Check docs/reference/exit-codes.md for the routing rule before "+
			"changing either side: most namespaces route whole, but storage/kms route to "+
			"ExitUnreachable only on their `unreachable` leaf.",
			len(bad), strings.Join(bad, "\n  "))
	}
}

// TestDocumentedErrorCodesExist is the leaf-level docs→code direction.
// The existing namespace guard runs code→docs and cannot see a
// documented leaf that no code path raises — it found `manifest.*` and
// `demo.*` missing from the page, but `wal.slot_create_failed` being ON
// the page and nowhere in the binary was invisible to it. (The real
// code is `wal.slot_ensure_failed`.)
//
// Scope is docs/reference/error-codes.md's TABLE ROWS, and the reason
// is that scope is where the claim is unambiguous. That page's tables
// exist to name codes; a backticked dotted token in a row is asserted
// to be one. Elsewhere the same shape means too many other things to
// judge, and a guard that cannot tell them apart invents failures:
// `recovery.signal` is a PostgreSQL file, `suggestion.command` is a
// field of the error envelope, and `wal.follower.leader_change` is a
// real EVENT assembled from a component and an op — none are error
// codes, all look exactly like one.
//
// Existence is checked against the raw Go sources rather than the AST
// scan of output.NewError, because codes legitimately reach helpers as
// literals — `kmsOpError(err, "kms rotate", "kms.rotate_failed", nil)`
// is real and the AST scan does not see it. Substring matching is the
// permissive direction, which is the right way for a guard to be wrong.
func TestDocumentedErrorCodesExist(t *testing.T) {
	root := repoRootFromOutput(t)
	sources := goSourceText(t, root)
	if len(sources) < 1_000_000 {
		t.Fatalf("read only %d bytes of Go source — the walk stopped working, so every "+
			"documented code would look present", len(sources))
	}

	page := filepath.Join("docs", "reference", "error-codes.md")
	var rows []docClaim
	walkDocFiles(t, root, func(rel string, lines []string) {
		if rel != filepath.ToSlash(page) {
			return
		}
		for i, line := range lines {
			if !strings.HasPrefix(strings.TrimSpace(line), "|") {
				continue // prose, not a code table row
			}
			for _, m := range docCodeRe.FindAllStringSubmatch(line, -1) {
				rows = append(rows, docClaim{
					code:  m[1] + "." + m[2],
					where: rel + ":" + strconv.Itoa(i+1),
				})
			}
		}
	})
	if len(rows) < 50 {
		t.Fatalf("found only %d code mentions in %s table rows — the page's shape changed "+
			"and this test is no longer reading it", len(rows), page)
	}

	var bad []string
	seen := map[string]bool{}
	for _, c := range rows {
		if strings.HasSuffix(c.code, ".*") || seen[c.code] {
			continue
		}
		if strings.Contains(sources, c.code) {
			continue
		}
		seen[c.code] = true
		bad = append(bad, c.where+"\n      `"+c.code+"` appears nowhere in the Go sources")
	}
	t.Logf("checked %d code mention(s) in %s", len(rows), page)

	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("%d error code(s) documented on the reference page are never raised:\n  %s\n\n"+
			"This page is where an operator looks up a code they hit, and where they get the "+
			"strings they alert on. A code listed here that the binary cannot emit is an "+
			"alert that never fires. Name the code that actually covers the situation.",
			len(bad), strings.Join(bad, "\n  "))
	}
}

// goSourceText concatenates the PRODUCTION Go sources once, so
// existence checks are a substring test rather than N tree walks.
//
// _test.go is excluded, and that exclusion is load-bearing rather than
// tidiness: this file's own comment names `wal.slot_create_failed` as
// the phantom it was written to catch. With tests included, that
// comment made the phantom look present and the guard passed when the
// drift was reintroduced — the test was masking the bug it exists to
// find. Any test naming a code it expects to be absent would do the
// same.
func goSourceText(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top),
			func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") ||
					strings.HasSuffix(path, "_test.go") {
					return nil
				}
				body, rerr := os.ReadFile(path)
				if rerr == nil {
					b.Write(body)
					b.WriteByte('\n')
				}
				return nil
			})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	return b.String()
}

// walkDocFiles hands each documentation page to fn as split lines.
func walkDocFiles(t *testing.T, root string, fn func(rel string, lines []string)) {
	t.Helper()
	err := filepath.WalkDir(filepath.Join(root, "docs"),
		func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil
			}
			body, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return nil
			}
			fn(filepath.ToSlash(rel), strings.Split(string(body), "\n"))
			return nil
		})
	if err != nil {
		t.Fatalf("walk docs: %v", err)
	}
}
