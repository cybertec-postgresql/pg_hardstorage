package server_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/leakcheck"
)

// A control plane that runs for months: stream a very large number of jobs
// (enqueue / claim / progress / complete, plus abandoned ones reclaimed by
// the sweeper) through the in-memory registry with a short retention, and
// check the job map is pruned and the heap and goroutines plateau.
func TestLeak_JobRegistryChurn(t *testing.T) {
	n := 20000
	if os.Getenv("PGHS_SCALE") == "1" {
		n = 400000
	}
	b := server.NewMemoryBackend()
	r := server.NewJobRegistryWithBackend(b).
		WithTerminalRetention(20 * time.Millisecond).
		WithClaimDeadline(30 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	base := leakcheck.Take()
	r.RunSweeper(ctx, 10*time.Millisecond, nil)
	leakcheck.Plateau(t, n, n/16, 8<<20, func(i int) {
		j, err := r.Enqueue(server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db1", RepoURL: "file:///srv/repo"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Claim(server.ClaimOptions{AgentID: "a", Deployments: []string{"db1"}}); err != nil {
			t.Fatal(err)
		}
		for k := 0; k < 3; k++ {
			_ = r.AppendProgress(j.ID, server.ProgressEvent{Op: "agent.progress", Body: map[string]any{"k": k}})
		}
		if i%5 != 0 { // 1 in 5 is abandoned and left for the sweeper
			if _, err := r.Complete(j.ID, server.CompleteOptions{Success: i%7 != 0}); err != nil {
				t.Fatal(err)
			}
		}
		if i%1000 == 0 {
			time.Sleep(60 * time.Millisecond) // let retention elapse so the sweeper prunes
		}
	})
	time.Sleep(200 * time.Millisecond)
	live, _ := r.List(server.ListOptions{})
	t.Logf("jobs still held after %d: %d", n, len(live))
	if len(live) > n/20 {
		t.Errorf("job map not pruned: %d of %d retained", len(live), n)
	}
	cancel()
	r.Stop()
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{})
}
