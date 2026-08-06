package gameday_test

// agentkill_test.go — the crash-recovery drill.
//
// The scenario previously reported Pass=true unconditionally while
// declaring an invariant about a supervisor re-execing an agent and
// reconciling state/inflight.json. None of that machinery exists, and
// the pg_backup_start leak it named cannot happen here: backups run
// BASE_BACKUP over a replication connection, which PostgreSQL tears
// down when the connection drops.
//
// What it drills now is the machinery that does exist — the backup
// lease — so these tests are about a lease being abandoned and
// reclaimed, not about signals.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/gameday"
)

func runAgentKillDrill(t *testing.T, opts gameday.RunOptions) *gameday.Result {
	t.Helper()
	res, err := gameday.Run(context.Background(), "agent_kill", opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// TestAgentKill_PassesOnABackendThatCanExclude is the happy path:
// file:// advertises conditional put, so the lease is enforceable and
// the drill should hold.
func TestAgentKill_PassesOnABackendThatCanExclude(t *testing.T) {
	res := runAgentKillDrill(t, gameday.RunOptions{RepoURL: "file://" + t.TempDir()})
	if !res.Pass {
		t.Fatalf("drill failed on a backend with atomic conditional put: %s\nevidence: %+v",
			res.Failure, res.Evidence)
	}
	if res.Deferred {
		t.Error("scenario still reports Deferred; it drives a real lease now")
	}
	if res.RecoveryTime <= 0 {
		t.Error("no RecoveryTime recorded — the drill cannot report how long reclaim took")
	}

	// The exclusion step is what makes the reclaim meaningful. Without
	// it, a lease that never excluded anyone would also "recover".
	var sawExclusion, sawRecovery bool
	for _, ev := range res.Evidence {
		if ev.Kind == "observed" && strings.Contains(ev.Message, "refused") {
			sawExclusion = true
		}
		if ev.Kind == "recovered" {
			sawRecovery = true
		}
	}
	if !sawExclusion {
		t.Error("no evidence that a second agent was refused while the abandoned lease was " +
			"live; a drill that only shows reclaim cannot distinguish a working lease from " +
			"one that excludes nobody")
	}
	if !sawRecovery {
		t.Error("no recovery evidence recorded")
	}
}

// TestAgentKill_ExactlyOneReclaimerWins is the property the drill
// exists for. Several agents observe the same expired lease; the break
// must be claimed atomically so only one proceeds, or crash recovery
// itself creates the concurrent-backup condition the lease prevents.
func TestAgentKill_ExactlyOneReclaimerWins(t *testing.T) {
	res := runAgentKillDrill(t, gameday.RunOptions{RepoURL: "file://" + t.TempDir()})
	if !res.Pass {
		t.Fatalf("drill failed: %s", res.Failure)
	}
	for _, ev := range res.Evidence {
		if ev.Kind != "recovered" {
			continue
		}
		w, ok := ev.Body["winners"]
		if !ok {
			t.Fatal("recovery evidence does not record how many agents won the race")
		}
		if fmt := toInt(w); fmt != 1 {
			t.Errorf("winners = %v, want exactly 1", w)
		}
		if n := toInt(ev.Body["reclaimers"]); n < 2 {
			t.Errorf("raced only %v agent(s); 'exactly one wins' is not a claim you can "+
				"make with fewer than two", ev.Body["reclaimers"])
		}
		return
	}
	t.Fatal("no recovery evidence")
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return -1
}

func TestAgentKill_RefusesWithoutARepository(t *testing.T) {
	res := runAgentKillDrill(t, gameday.RunOptions{})
	if res.Pass {
		t.Fatal("drill passed with no repository — it abandoned nothing and reclaimed nothing")
	}
	if !res.Misconfigured {
		t.Error("a missing --repo is a configuration problem; without Misconfigured the " +
			"CLI reports verify.failed (exit 9)")
	}
}

// TestAgentKill_RefusesAnImpossibleBudget: recover_within shorter than
// the lease TTL cannot observe a reclaim, because the reclaim is not
// permitted to happen yet. Saying so beats timing out.
func TestAgentKill_RefusesAnImpossibleBudget(t *testing.T) {
	res := runAgentKillDrill(t, gameday.RunOptions{
		RepoURL:       "file://" + t.TempDir(),
		RecoverWithin: 100 * time.Millisecond,
	})
	if res.Pass {
		t.Fatal("drill passed with a recovery budget shorter than the lease TTL")
	}
	if !res.Misconfigured {
		t.Error("an impossible budget is the operator's parameter, not a product failure")
	}
}

// TestAgentKill_CleansUpAfterItself — the drill writes lease objects
// into a real repository.
func TestAgentKill_CleansUpAfterItself(t *testing.T) {
	dir := t.TempDir()
	res := runAgentKillDrill(t, gameday.RunOptions{RepoURL: "file://" + dir})
	if !res.Pass {
		t.Fatalf("drill failed: %s", res.Failure)
	}
	var left []string
	_ = filepath.Walk(filepath.Join(dir, "leases"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.Contains(path, "__gameday_agentkill_probe") {
			left = append(left, path)
		}
		return nil
	})
	if len(left) > 0 {
		t.Errorf("drill left %d lease object(s) behind: %v — a live probe lease would "+
			"block a real backup of that name until its TTL elapsed", len(left), left)
	}
}

func TestAgentKill_DryRunTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	res := runAgentKillDrill(t, gameday.RunOptions{RepoURL: "file://" + dir, DryRun: true})
	if !res.Pass {
		t.Fatalf("dry run failed: %s", res.Failure)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("dry run wrote %d entry/entries: %v", len(entries), entries)
	}
}
