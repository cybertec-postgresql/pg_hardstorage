package cli_test

// root_context_test.go — cli.Run must honour a context the caller set.
//
// Run installs a signal handler so Ctrl-C cancels the command context
// instead of killing the process. It used to build that context from
// context.Background() and SetContext it onto the root, DISCARDING
// anything the caller had already put there.
//
// Nothing in production sets one, so the discard was invisible. It was
// not invisible to tests: internal/restore's PITR test wrapped
// `wal stream --once` in a 60s deadline, the deadline was dropped here,
// and when the stream's segment never landed the run never returned. It
// consumed the whole 15-minute package budget and go test killed
// internal/restore, reporting one arbitrary test and hiding every other
// test in the package behind "panic: test timed out".
//
// Deriving the signal context from the caller's — rather than replacing
// it — keeps production behaviour identical and makes the command
// interruptible by whoever invoked it.

import (
	"context"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
)

// blockingCmd is a hidden command that blocks until its context ends.
// Added to a per-test root, so it never reaches the real command tree.
func blockingCmd(observed chan<- context.Context) *cobra.Command {
	return &cobra.Command{
		Use:    "test-block-until-cancelled",
		Hidden: true,
		RunE: func(c *cobra.Command, _ []string) error {
			observed <- c.Context()
			<-c.Context().Done()
			return c.Context().Err()
		},
	}
}

// TestRun_HonoursCallerContext is the regression test: a deadline set
// by the caller must actually stop the command.
func TestRun_HonoursCallerContext(t *testing.T) {
	root := cli.NewRoot()
	observed := make(chan context.Context, 1)
	root.AddCommand(blockingCmd(observed))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	root.SetContext(ctx)
	root.SetArgs([]string{"test-block-until-cancelled"})

	done := make(chan int, 1)
	start := time.Now()
	go func() { done <- cli.Run(root) }()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
			t.Errorf("returned after %s, before the caller's 300ms deadline", elapsed)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cli.Run did not return when the caller's context expired — the context " +
			"was discarded, so nothing the caller does can interrupt a blocking command. " +
			"A test that wraps a long-running command in a deadline gets no deadline, and " +
			"a stall consumes its whole package budget")
	}
}

// TestRun_CallerContextReachesTheCommand pins the mechanism rather than
// just the outcome: the command's own ctx must be a DESCENDANT of the
// caller's, so cancelling the caller's cancels the command's.
//
// Checking only that Run returns would also pass if Run ignored the
// caller and happened to exit for some other reason.
func TestRun_CallerContextReachesTheCommand(t *testing.T) {
	root := cli.NewRoot()
	observed := make(chan context.Context, 1)
	root.AddCommand(blockingCmd(observed))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root.SetContext(ctx)
	root.SetArgs([]string{"test-block-until-cancelled"})

	done := make(chan int, 1)
	go func() { done <- cli.Run(root) }()

	var cmdCtx context.Context
	select {
	case cmdCtx = <-observed:
	case <-time.After(30 * time.Second):
		t.Fatal("command never ran")
	}
	if cmdCtx.Err() != nil {
		t.Fatalf("command context already done on entry: %v", cmdCtx.Err())
	}

	cancel() // cancelling the CALLER's context must reach the command

	select {
	case <-cmdCtx.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("the command's context did not end when the caller's was cancelled — " +
			"Run built its context from Background instead of deriving it, so the two are " +
			"unrelated")
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("cli.Run did not return after cancellation")
	}
}

// TestRun_WithoutCallerContextStillWorks is the production path: no
// caller sets a context, so Run must supply its own rather than
// dereferencing a nil one.
func TestRun_WithoutCallerContextStillWorks(t *testing.T) {
	root := cli.NewRoot()
	observed := make(chan context.Context, 1)
	root.AddCommand(blockingCmd(observed))
	root.SetArgs([]string{"test-block-until-cancelled"})

	done := make(chan int, 1)
	go func() { done <- cli.Run(root) }()

	select {
	case cmdCtx := <-observed:
		if cmdCtx == nil {
			t.Fatal("command received a nil context")
		}
		if cmdCtx.Err() != nil {
			t.Fatalf("context already done: %v", cmdCtx.Err())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("command never ran with no caller context set")
	}

	// Nothing will cancel it; the goroutine is left blocked and the
	// process exits with the test binary. Asserting it ran with a live
	// context is the point.
}
