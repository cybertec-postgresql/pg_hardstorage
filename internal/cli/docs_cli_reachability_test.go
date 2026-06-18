// docs_cli_reachability_test.go — meta-test pinning that every
// `pg_hardstorage <subcommand>` invocation in docs/ references
// a subcommand that actually exists.
//
// Issue #98 (metrics) and the plugin-list drift caught while
// writing this test share a pattern: docs advertise a CLI
// surface that the binary doesn't provide.  An operator
// following the docs hits an "unknown command" error.  This
// test catches that drift at build time so it can never ship.
//
// Scope:
//
//   - Only fenced code blocks tagged with bash|sh|console are
//     scanned (plain prose / unlabeled fences are skipped — they
//     match too many false positives from changelog narrative).
//   - Only invocations where `pg_hardstorage` is at line-start
//     OR preceded by `$ ` (shell prompt) — filters out prose
//     mentions and embedded usernames (`mc alias set ...
//     pg_hardstorage <secret>` from kubernetes-cnpg).
//   - Only the FIRST token after `pg_hardstorage` is checked
//     against the cobra registration list.  Subcommand
//     hierarchies (e.g. `manifest show`) are validated at the
//     first-level only here; second-level is a follow-up.
//
// If this test fails, EITHER:
//
//	(a) the docs reference a renamed/removed subcommand → fix
//	    the docs to use the current name, OR
//	(b) the docs reference a planned-but-unshipped subcommand →
//	    implement it (the plugin list addition is this branch).
package cli_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
)

// docsRoot finds the repo's docs/ dir relative to this test
// file so the test works from any cwd.
func docsRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/cli → ../../docs
	return filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", "docs"))
}

// knownSubcommands returns the set of first-level subcommands
// cobra knows about.  This is the source of truth — anything
// the docs claim must be in this set.
func knownSubcommands(t *testing.T) map[string]bool {
	t.Helper()
	root := cli.NewRoot()
	out := map[string]bool{}
	for _, c := range root.Commands() {
		out[c.Name()] = true
		// also accept aliases so docs using e.g. `ls` for
		// `list` (if such aliases exist) don't false-fail
		for _, a := range c.Aliases {
			out[a] = true
		}
	}
	return out
}

// shellFenceRe matches fenced code blocks explicitly tagged as
// shell (bash | sh | console).  Plain or other-language fences
// are intentionally not matched — they're prose-prone.
var shellFenceRe = regexp.MustCompile("(?s)```(?:bash|sh|console)\\s*\\n(.*?)\\n```")

// invocationRe matches `pg_hardstorage <sub>` where:
//   - `pg_hardstorage` is at line-start OR right after a shell
//     prompt `$ ` (handles both copy-paste-as-prompt and
//     no-prompt block styles).
//   - `<sub>` starts with a lowercase letter and contains only
//     [a-z_-] (subcommands are kebab-case lowercase).
var invocationRe = regexp.MustCompile(`(?m)(?:^|\$\s+)pg_hardstorage\s+([a-z][a-z_-]+)(?:\s|$|\\)`)

// TestDocsCLIReachability_AllSubcommandsExist: every
// `pg_hardstorage <X>` in fenced shell blocks must reference an
// X that cobra knows about.
//
// Currently-known false-positive sources (excluded by callers
// in the regex above):
//   - Headings inside fenced blocks (`## ...`)
//   - mc alias passwords (`mc alias set ... pg_hardstorage <secret>`)
//   - Prose verbs (`pg_hardstorage is/cannot/stores ...`)
func TestDocsCLIReachability_AllSubcommandsExist(t *testing.T) {
	known := knownSubcommands(t)
	if !known["plugin"] {
		// Sanity: this test will fail unhelpfully if cobra
		// registration broke entirely.  Fail FAST with a
		// clear hint instead.
		t.Fatal("internal/cli/root.go's AddCommand list seems empty — known subcommands missing")
	}
	type miss struct {
		file string
		sub  string
	}
	var missing []miss

	err := filepath.Walk(docsRoot(t), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, fence := range shellFenceRe.FindAllSubmatch(body, -1) {
			block := fence[1]
			for _, m := range invocationRe.FindAllSubmatch(block, -1) {
				sub := string(m[1])
				if !known[sub] {
					missing = append(missing, miss{path, sub})
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs: %v", err)
	}

	if len(missing) > 0 {
		// Group by subcommand for a readable error.
		bySub := map[string][]string{}
		for _, m := range missing {
			rel, _ := filepath.Rel(docsRoot(t), m.file)
			bySub[m.sub] = append(bySub[m.sub], rel)
		}
		var keys []string
		for k := range bySub {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var lines []string
		for _, k := range keys {
			files := bySub[k]
			sort.Strings(files)
			lines = append(lines, "  "+k+" — referenced in "+strings.Join(files, ", "))
		}
		t.Errorf("%d unknown subcommand(s) referenced in docs.\n"+
			"Either implement them in internal/cli/ OR update the docs to use\n"+
			"the current name:\n%s",
			len(bySub), strings.Join(lines, "\n"))
	}
}
