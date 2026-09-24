package logical_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical"
)

// A server-side CopyDone (walsender shutdown, PG restart) makes the
// receiver return nil while the runner's ctx is still live. The
// supervisor used to treat that as terminal and exit — and because the
// stream's name stayed in the watcher's active map, no rescan ever
// restarted it: CDC silently stopped until the agent was restarted.
func TestSupervisor_RestartsAfterServerCopyDone(t *testing.T) {
	mgr := newRunnerManager(t)
	if _, err := mgr.Add(logical.AddOptions{
		Name: "cdc", Deployment: "db1", Slot: "s1", Plugin: "pgoutput",
		Publication: "pub", SinkKind: "chunked", RepoURL: "file:///tmp/whatever",
	}); err != nil {
		t.Fatal(err)
	}

	var attempts atomic.Int32
	restarted := make(chan struct{})
	r := &logical.Runner{
		Manager:        mgr,
		ConnectionFor:  func(*logical.Stream) string { return "host=x" },
		RescanInterval: 10 * time.Millisecond,
		Backoff:        logical.Backoff{Initial: time.Millisecond, Max: time.Millisecond},
	}
	logical.SetRunOnce(r, func(ctx context.Context, _ *logical.Stream, _ string) error {
		if attempts.Add(1) == 1 {
			return nil // clean server-side end of COPY
		}
		close(restarted)
		<-ctx.Done()
		return ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()

	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatalf("stream was not restarted after a clean server CopyDone (attempts=%d)", attempts.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after cancel")
	}
}
