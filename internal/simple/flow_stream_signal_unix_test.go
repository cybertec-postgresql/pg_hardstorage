//go:build unix

package simple

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

// Cancelling the stream flow (the operator's Ctrl-C) used to SIGKILL
// the wal-stream child — exec.CommandContext's default Cancel — so its
// graceful drain (pg_switch_wal, final segment flush) never ran. The
// child must get an interrupt it can handle.
func TestFlowStream_CancelInterruptsChildGracefully(t *testing.T) {
	dir := t.TempDir()
	mark := filepath.Join(dir, "mark")
	started := filepath.Join(dir, "started")
	fake := filepath.Join(dir, "pg_hardstorage")
	script := "#!/bin/sh\n" +
		"trap 'echo graceful > \"" + mark + "\"; exit 0' INT TERM\n" +
		"echo up > \"" + started + "\"\n" +
		"while :; do sleep 0.05; done\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PG_HARDSTORAGE_BIN", fake)

	env, _ := driveEnv(t, "y\n")
	env.Config = &config.LoadResult{}
	env.Config.Config.Deployments = map[string]config.DeploymentConfig{
		"db1": {PGConnection: "postgres://h/db", Repo: "file:///srv/repo"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- flowStream{}.Run(ctx, env) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake wal stream never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("flow returned %v after an operator stop; want a clean stop", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("flow did not return after cancel")
	}
	body, _ := os.ReadFile(mark)
	if !strings.Contains(string(body), "graceful") {
		t.Fatal("wal-stream child was killed without a catchable signal; its graceful drain never ran")
	}
}

// fakeFlow lets the dispatch test observe the context each flow gets.
type fakeFlow struct{ run func(ctx context.Context) }

func (fakeFlow) Name() string { return "fake" }
func (f fakeFlow) Run(ctx context.Context, _ *Env) error {
	f.run(ctx)
	return nil
}

// Each flow gets its own interrupt-cancellable context: a Ctrl-C stops
// the running flow — and only that flow. The session used to share one
// signal.NotifyContext, so the first Ctrl-C left every later flow
// starting with an already-cancelled context.
func TestDispatch_InterruptCancelsOnlyTheRunningFlow(t *testing.T) {
	// Safety net: whatever dispatch does, a SIGINT must not kill the
	// test binary.
	guard := make(chan os.Signal, 4)
	signal.Notify(guard, os.Interrupt)
	defer signal.Stop(guard)

	var firstCancelled bool
	var secondErr error = context.Canceled
	saved := menuChoices
	defer func() { menuChoices = saved }()
	menuChoices = []struct {
		Label string
		Flow  Flow
	}{
		{Label: "interrupt me", Flow: fakeFlow{run: func(ctx context.Context) {
			_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
			select {
			case <-ctx.Done():
				firstCancelled = true
			case <-time.After(2 * time.Second):
			}
		}}},
		{Label: "run after", Flow: fakeFlow{run: func(ctx context.Context) { secondErr = ctx.Err() }}},
	}

	env, _ := driveEnv(t, "1\n2\nq\n")
	if err := Run(context.Background(), env); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !firstCancelled {
		t.Error("Ctrl-C did not cancel the running flow's context")
	}
	if secondErr != nil {
		t.Errorf("the next flow started with a dead context (%v)", secondErr)
	}
}
