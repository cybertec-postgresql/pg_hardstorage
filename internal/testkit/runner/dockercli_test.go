package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeDockerOnPath installs an executable named `docker` at the front
// of PATH, so the helpers under test resolve to it the way they
// resolve the real one. Testing through PATH rather than through an
// injected command name keeps the test honest: it exercises the
// literal exec.CommandContext(tctx, "docker", ...) that shipped.
func fakeDockerOnPath(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestDockerDiagDoesNotHangForever is the regression for the wedge
// that cost a day: a `scenario run` blocked 1553 minutes inside a
// docker CLI call, holding its container and its log file, because
// nothing on that path carried a deadline.
//
// The forensic calls cannot take the run's context — see dockerDiag —
// so if they do not bound themselves, nothing does.
func TestDockerDiagDoesNotHangForever(t *testing.T) {
	fakeDockerOnPath(t, "sleep 600")

	old := dockerCLITimeout
	dockerCLITimeout = 250 * time.Millisecond
	defer func() { dockerCLITimeout = old }()

	done := make(chan struct{})
	go func() {
		_, _ = dockerDiag("logs", "--tail", "200", "whatever")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("dockerDiag never returned against a docker that hangs — " +
			"this is the 1553-minute wedge, and an unbounded forensic call " +
			"reintroduces it even when every other path is bounded")
	}
}

// TestDockerInfoDoesNotHangForever covers the call that actually hung:
// findDockerLeaderContainer's `docker ps`. It already used
// CommandContext, which bought nothing, because the context a scenario
// run hands down has no deadline. The bound has to be the helper's
// own.
func TestDockerInfoDoesNotHangForever(t *testing.T) {
	fakeDockerOnPath(t, "sleep 600")

	old := dockerCLITimeout
	dockerCLITimeout = 250 * time.Millisecond
	defer func() { dockerCLITimeout = old }()

	// context.Background() stands in for cobra's: no deadline, ever.
	start := time.Now()
	_, err := dockerInfo(context.Background(), "ps", "--format", "{{.Names}}")
	if err == nil {
		t.Fatal("a docker that never answers must surface as an error, not as success")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("dockerInfo took %s against a hanging docker; the deadline did not apply", elapsed)
	}
}

// TestDockerInfoHonoursCallerCancellation pins the difference between
// the two helpers. dockerInfo is on the happy path, so a cancelled run
// should abandon it immediately rather than wait out the full
// dockerCLITimeout.
func TestDockerInfoHonoursCallerCancellation(t *testing.T) {
	fakeDockerOnPath(t, "sleep 600")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled, as a torn-down run would be

	start := time.Now()
	if _, err := dockerInfo(ctx, "ps"); err == nil {
		t.Fatal("expected an error from an already-cancelled context")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("dockerInfo ignored the caller's cancellation (took %s)", elapsed)
	}
}

// TestDockerDiagIgnoresCallerCancellation is the other half, and the
// reason dockerDiag takes no context at all. These calls run *because*
// something failed, at which point the run's context is usually
// already dead. Scraping logs under it would return nothing and
// discard the only explanation the operator gets.
func TestDockerDiagIgnoresCallerCancellation(t *testing.T) {
	fakeDockerOnPath(t, "echo FATAL: could not start postmaster")

	out, err := dockerDiag("logs", "--tail", "200", "sandbox")
	if err != nil {
		t.Fatalf("dockerDiag: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("dockerDiag returned no output; forensics on a failure path must still be collected")
	}
}
