package cli_test

// duration_flags_consistency_test.go — one binary, one duration grammar.
//
// Issue #52's shape keeps coming back: a flag whose help text (or whose
// sibling command) speaks in days while its parser stops at hours. The
// fix for --keep-for/--horizon/--keep-since missed four more:
//
//   - repo gc --tombstone-grace: wal prune's twin flag — documented
//     "matches repo gc", and REQUIRED to agree with it (prune keeps a
//     tombstoned backup's WAL while gc keeps its chunks; if the two
//     values drift, undelete breaks) — accepted "2d" while gc rejected
//     it. A pair of flags that must hold the same value but accept
//     different spellings of it invites exactly the drift they exist
//     to prevent.
//   - repo gc --min-chunk-age: same command, same retention scale.
//   - gameday report --since: its own help says "default 90d"; the
//     parser rejected "90d".
//   - dsa/integrity/insider list --since: the only --since flags in
//     the binary that took RFC3339 ONLY, so the "7d" that works on
//     audit/compliance/drill-history was rejected here.
//
// These tests run the real commands end-to-end so the grammar is
// pinned at the CLI boundary, not at the parser function.

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func TestRepoGC_RetentionFlagsAcceptDayUnits(t *testing.T) {
	w := newReadWorld(t)
	stdout, stderr, exit := runCLI(t, "repo", "gc",
		"--repo", w.repoURL,
		"--tombstone-grace", "2d",
		"--min-chunk-age", "1d",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("repo gc with day-unit retention flags: exit = %d\nstdout: %s\nstderr: %s",
			exit, stdout, stderr)
	}
}

func TestGamedayReport_SinceAcceptsItsOwnDocumentedDefault(t *testing.T) {
	w := newReadWorld(t)
	// "90d" is the default the flag's help text advertises. It must
	// parse — a flag whose own default is unspellable is worse than
	// an undocumented one.
	stdout, stderr, exit := runCLI(t, "gameday", "report",
		"--repo", w.repoURL,
		"--since", "90d",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("gameday report --since 90d: exit = %d\nstdout: %s\nstderr: %s",
			exit, stdout, stderr)
	}
}

func TestListCommands_SinceGrammarMatchesAuditFamily(t *testing.T) {
	w := newReadWorld(t)
	for _, cmd := range [][]string{
		{"dsa", "list"},
		{"integrity", "list"},
		{"insider", "list"},
	} {
		name := strings.Join(cmd, " ")
		// The duration spelling that every other --since accepts.
		args := append(append([]string{}, cmd...),
			"--repo", w.repoURL, "--since", "7d", "-o", "json")
		if stdout, stderr, exit := runCLI(t, args...); exit != int(output.ExitOK) {
			t.Errorf("%s --since 7d: exit = %d\nstdout: %s\nstderr: %s", name, exit, stdout, stderr)
		}
		// RFC3339 must keep working — the grammar widened, it did not move.
		args = append(append([]string{}, cmd...),
			"--repo", w.repoURL, "--since", "2026-04-01T00:00:00Z", "-o", "json")
		if stdout, stderr, exit := runCLI(t, args...); exit != int(output.ExitOK) {
			t.Errorf("%s --since RFC3339: exit = %d\nstdout: %s\nstderr: %s", name, exit, stdout, stderr)
		}
		// Garbage must still be refused as usage, not swallowed.
		args = append(append([]string{}, cmd...),
			"--repo", w.repoURL, "--since", "notatime", "-o", "json")
		if _, _, exit := runCLI(t, args...); exit != int(output.ExitMisuse) {
			t.Errorf("%s --since notatime: exit = %d, want %d (usage)", name, exit, int(output.ExitMisuse))
		}
	}
}
