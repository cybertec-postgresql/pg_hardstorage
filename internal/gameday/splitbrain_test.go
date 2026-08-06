package gameday_test

// splitbrain_test.go — the split-brain drill against a real file://
// repository.
//
// These run against a temp-dir repo rather than a fake plugin so the
// push path exercised is the one archive_command actually takes:
// chunking, CAS commit, manifest write, and the existing-manifest
// verification that does the refusing.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/gameday"
)

func splitBrainRepo(t *testing.T) string {
	t.Helper()
	return "file://" + t.TempDir()
}

func runSplitBrain(t *testing.T, opts gameday.RunOptions) *gameday.Result {
	t.Helper()
	res, err := gameday.Run(context.Background(), "patroni_split_brain", opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// TestSplitBrain_PassesWhenTheRepositoryRefuses is the happy path: the
// product does refuse, so the drill passes.
func TestSplitBrain_PassesWhenTheRepositoryRefuses(t *testing.T) {
	res := runSplitBrain(t, gameday.RunOptions{RepoURL: splitBrainRepo(t)})
	if !res.Pass {
		t.Fatalf("drill failed against a repository that should refuse: %s\nevidence: %+v",
			res.Failure, res.Evidence)
	}

	// Both refusals must be recorded, not just one — the drill covers
	// the same-cluster and foreign-cluster cases separately and a pass
	// that only exercised one would be half a drill.
	var refusals int
	for _, ev := range res.Evidence {
		if ev.Kind == "refused" {
			refusals++
		}
	}
	if refusals != 2 {
		t.Errorf("recorded %d refusal(s), want 2 (content mismatch + foreign cluster); "+
			"evidence: %+v", refusals, res.Evidence)
	}
}

// TestSplitBrain_RefusesWithoutARepository: the drill writes into a
// repository, so it cannot run without one — and must say so as a
// configuration problem rather than a failed invariant.
func TestSplitBrain_RefusesWithoutARepository(t *testing.T) {
	res := runSplitBrain(t, gameday.RunOptions{})
	if res.Pass {
		t.Fatal("drill passed with no repository — it archived nothing and contended " +
			"with nothing")
	}
	if !res.Misconfigured {
		t.Error("a missing --repo is a configuration problem; without Misconfigured the " +
			"CLI reports verify.failed (exit 9), which reads as 'your backups are bad'")
	}
}

// TestSplitBrain_CleansUpAfterItself is the safety property. The drill
// writes probe WAL into the operator's real repository; leaving it
// there is not acceptable.
func TestSplitBrain_CleansUpAfterItself(t *testing.T) {
	dir := t.TempDir()
	res := runSplitBrain(t, gameday.RunOptions{RepoURL: "file://" + dir})
	if !res.Pass {
		t.Fatalf("drill failed: %s", res.Failure)
	}

	// Nothing may remain under the probe deployment's WAL prefix.
	var left []string
	walRoot := filepath.Join(dir, "wal")
	_ = filepath.Walk(walRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.Contains(path, "__gameday_splitbrain_probe") {
			left = append(left, path)
		}
		return nil
	})
	if len(left) > 0 {
		t.Errorf("drill left %d probe object(s) in the repository: %v\n\n"+
			"This runs against production repositories. Probe WAL accumulating there is "+
			"the kind of debris that later reads as a real deployment nobody remembers "+
			"configuring.", len(left), left)
	}

	var cleaned bool
	for _, ev := range res.Evidence {
		if ev.Kind == "cleanup" {
			cleaned = true
		}
		if ev.Kind == "cleanup_failed" {
			t.Errorf("cleanup reported a failure: %s", ev.Message)
		}
	}
	if !cleaned {
		t.Error("no cleanup evidence recorded; an operator cannot tell whether the drill " +
			"tidied up")
	}
}

// TestSplitBrain_DryRunTouchesNothing: --dry-run must not write to the
// repository at all. An operator previewing a drill against production
// has every right to expect that.
func TestSplitBrain_DryRunTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	res := runSplitBrain(t, gameday.RunOptions{RepoURL: "file://" + dir, DryRun: true})
	if !res.Pass {
		t.Fatalf("dry run failed: %s", res.Failure)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("dry run wrote %d entry/entries into the repository: %v", len(entries), entries)
	}
}
