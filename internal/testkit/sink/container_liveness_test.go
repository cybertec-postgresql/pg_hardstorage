// In-package tests for ensureContainerRunning: the helper polls
// `docker inspect`, so the tests stub the package's execCommand
// var with `cat` of pre-written files — no shell quoting involved.
package sink

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubDocker swaps execCommand so that `docker inspect` returns
// successive canned states and `docker logs` returns a canned log
// tail. inspectStates is consumed one entry per poll; the final
// entry repeats once exhausted.
func stubDocker(t *testing.T, inspectStates []string, logTail string) (restore func()) {
	t.Helper()
	dir := t.TempDir()
	stateFiles := make([]string, 0, len(inspectStates))
	for i, s := range inspectStates {
		p := filepath.Join(dir, fmt.Sprintf("inspect-%d", i))
		if err := os.WriteFile(p, []byte(s+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		stateFiles = append(stateFiles, p)
	}
	logFile := filepath.Join(dir, "logs")
	if err := os.WriteFile(logFile, []byte(logTail+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := execCommand
	calls := 0
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "docker" {
			switch args[0] {
			case "inspect":
				i := calls
				if i >= len(stateFiles) {
					i = len(stateFiles) - 1
				}
				calls++
				return exec.Command("cat", stateFiles[i])
			case "logs":
				return exec.Command("cat", logFile)
			}
		}
		return exec.Command(name, args...)
	}
	return func() { execCommand = orig }
}

func TestEnsureContainerRunning_ExitedReturnsTypedError(t *testing.T) {
	restore := stubDocker(t,
		[]string{"exited\t255"},
		"exec /entrypoint: exec format error")
	defer restore()

	err := ensureContainerRunning(context.Background(), "pg-hs-test", 10*time.Second)
	if err == nil {
		t.Fatal("expected error for exited container")
	}
	var ce *ContainerExitError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *ContainerExitError, got %T: %v", err, err)
	}
	if ce.Container != "pg-hs-test" {
		t.Errorf("Container = %q, want pg-hs-test", ce.Container)
	}
	if ce.ExitCode != 255 {
		t.Errorf("ExitCode = %d, want 255", ce.ExitCode)
	}
	if !strings.Contains(ce.Log, "exec format error") {
		t.Errorf("Log = %q, want it to carry the docker log tail", ce.Log)
	}
	// The platform-mismatch verdict is what lets a test SKIP an
	// amd64-only fixture on an arm64 host instead of failing it.
	if !ce.IsPlatformMismatch() {
		t.Error("IsPlatformMismatch = false, want true for exec format error")
	}
}

func TestEnsureContainerRunning_RunningAfterCreatedPollsToSuccess(t *testing.T) {
	// "created" is transient (the entrypoint has not exec'd yet):
	// the helper must keep polling rather than fail fast.
	restore := stubDocker(t,
		[]string{"created\t0", "running\t0"},
		"")
	defer restore()

	if err := ensureContainerRunning(context.Background(), "pg-hs-test", 10*time.Second); err != nil {
		t.Fatalf("expected nil after created→running, got %v", err)
	}
}

func TestEnsureContainerRunning_NonPlatformExitIsNotMismatch(t *testing.T) {
	// A fixture that crashes for its own reasons (bad entrypoint
	// script, missing binary) must NOT be classified as a platform
	// mismatch — tests fail loud on those.
	restore := stubDocker(t,
		[]string{"exited\t1"},
		"/entrypoint: line 3: ./bin/server: No such file or directory")
	defer restore()

	err := ensureContainerRunning(context.Background(), "pg-hs-test", 10*time.Second)
	var ce *ContainerExitError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *ContainerExitError, got %v", err)
	}
	if ce.IsPlatformMismatch() {
		t.Error("IsPlatformMismatch = true for a plain entrypoint crash")
	}
}

func TestEnsureContainerRunning_ContextCancelAborts(t *testing.T) {
	// A container stuck in "created" past the caller's context
	// budget must abort with the context error, not the budget error.
	restore := stubDocker(t,
		[]string{"created\t0"},
		"")
	defer restore()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := ensureContainerRunning(ctx, "pg-hs-test", 30*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestEnsureContainerRunning_BudgetElapsesWithoutRunning(t *testing.T) {
	// Transient inspect failures (here: every poll fails) must end
	// in a bounded error naming the container, not a hang.
	orig := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.Command("sh", "-ec", "exit 1") // inspect always fails
	}
	defer func() { execCommand = orig }()

	start := time.Now()
	err := ensureContainerRunning(context.Background(), "pg-hs-test", 2*time.Second)
	if err == nil {
		t.Fatal("expected error when container never reports running")
	}
	if !strings.Contains(err.Error(), "pg-hs-test") {
		t.Errorf("error %q should name the container", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("gave up after %v; budget was 2s", time.Since(start))
	}
}
