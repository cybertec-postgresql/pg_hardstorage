package cli_test

// coverage_allowlist_test.go — the CLI coverage gate's allow-list must
// not rot.
//
// The gate walks the live command tree and consults the allow-list only
// for leaves it FOUND. An entry naming a command that no longer exists
// is therefore never looked at: it cannot fail, cannot be reported, and
// stays in the file indefinitely. Two ways that bites:
//
//   - a renamed command silently loses its exemption's rationale, and
//     the comment above the stale entry — the record of WHY something
//     is exempt — now documents nothing;
//   - if the name is ever reused for a real command, it arrives
//     exempt from coverage on day one, which is the opposite of what
//     an allow-list is for.
//
// The allow-list is small and hand-maintained with a comment per entry,
// so it is exactly the kind of file that drifts quietly.

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// repoRootFromTest locates the checkout root from this file's path.
func repoRootFromTest(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
}

// loadCoverageAllowlist reads the same file, with the same syntax, as
// cmd/pg_hardstorage_testkit's loadAllowlist: one leaf per line, '#'
// comments and blanks skipped.
func loadCoverageAllowlist(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open allow-list: %v", err)
	}
	defer f.Close()

	var entries []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entries = append(entries, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read allow-list: %v", err)
	}
	return entries
}

// TestCoverageAllowlist_EntriesStillExist is the rot guard.
//
// The command tree comes from the same __dump-cmd-tree the gate uses,
// and group_guard commands are excluded here for the same reason the
// gate excludes them: they are not leaves, so allow-listing one would
// be meaningless.
func TestCoverageAllowlist_EntriesStillExist(t *testing.T) {
	nodes := dumpTree(t)

	leaves := map[string]bool{}
	for _, n := range nodes {
		if n.Hidden || n.GroupGuard || !n.Runnable {
			continue
		}
		leaves[n.Path] = true
	}
	if len(leaves) == 0 {
		t.Fatal("no leaf commands found; this test would pass vacuously")
	}

	root := repoRootFromTest(t)
	allowPath := filepath.Join(root, ".testkit", "cli_coverage_allowlist.txt")
	entries := loadCoverageAllowlist(t, allowPath)
	if len(entries) == 0 {
		t.Skip("allow-list is empty — nothing to keep honest")
	}

	var stale []string
	for _, e := range entries {
		if !leaves[e] {
			stale = append(stale, e)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("%d allow-list entr(y/ies) name no command in the tree: %v\n\n"+
			"The gate only consults the allow-list for leaves it found, so these are dead "+
			"lines: they can never fail and never be reported. Remove them, or fix the "+
			"name if the command was renamed. If one of these names is ever reused for a "+
			"real command, it would arrive exempt from coverage on its first day.\n\n"+
			"Allow-list: %s", len(stale), stale, allowPath)
	}
}

// TestCoverageAllowlist_EntriesAreDocumented pins the review
// convention the file's own header states: a one-line comment above
// each entry explaining why it is exempt.
//
// Without the reason, nobody can tell later whether an exemption is
// still justified, and the safe-looking choice is always to leave it —
// so exemptions accumulate and the gate quietly measures less over
// time.
func TestCoverageAllowlist_EntriesAreDocumented(t *testing.T) {
	root := repoRootFromTest(t)
	path := filepath.Join(root, ".testkit", "cli_coverage_allowlist.txt")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read allow-list: %v", err)
	}

	var undocumented []string
	var prevComment bool
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
			// A blank line separates groups; it does not carry a reason
			// forward to the next entry.
			prevComment = false
		case strings.HasPrefix(line, "#"):
			prevComment = true
		default:
			if !prevComment {
				undocumented = append(undocumented, line)
			}
			prevComment = false
		}
	}
	if len(undocumented) > 0 {
		t.Errorf("allow-list entries with no preceding comment: %v\n\n"+
			"The file's own header asks for a one-line reason above each entry. Without "+
			"one, a later reviewer cannot judge whether the exemption still holds, and "+
			"leaving it is always the safer-looking call — so the gate measures less and "+
			"less over time.\n\nAllow-list: %s", undocumented, path)
	}
}
