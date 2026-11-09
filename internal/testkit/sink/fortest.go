package sink

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// Budgets for a test's sink. A healthy emulator is up in seconds; these
// only bound the unhealthy case, where an unbounded `docker run` or a
// suite that pays the full readiness budget once per sub-case used to
// run into the package timeout instead of failing.
const (
	dockerProbeBudget = 20 * time.Second
	// A container start, not just an API answer: a daemon whose
	// storage has stalled still answers `docker info` while every
	// `docker run` hangs. Alpine is ~3 MB, so a first-time pull fits.
	dockerRunProbeBudget = 60 * time.Second
	dockerRunProbeImage  = "alpine:3.20"
	testSinkUpBudget     = 3 * time.Minute
	// After this many consecutive failed starts of one kind, later
	// tests in the process fail at once instead of waiting again. Two,
	// not one: a single slow start on a loaded host is routine.
	testSinkGiveUpAfter = 2
)

var (
	dockerProbeOnce sync.Once
	dockerProbeErr  error

	dockerRunProbeOnce sync.Once
	dockerRunProbeErr  error

	testSinkMu       sync.Mutex
	testSinkFailures = map[string]int{}
	testSinkLastErr  = map[string]error{}
)

// dockerAvailable reports, once per process, whether a Docker daemon
// answers within dockerProbeBudget.
func dockerAvailable() error {
	dockerProbeOnce.Do(func() {
		if _, err := exec.LookPath("docker"); err != nil {
			dockerProbeErr = fmt.Errorf("docker not on PATH: %w", err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), dockerProbeBudget)
		defer cancel()
		if err := execCommand(ctx, "docker", "info").Run(); err != nil {
			if ctx.Err() != nil {
				err = fmt.Errorf("no answer within %s", dockerProbeBudget)
			}
			dockerProbeErr = fmt.Errorf("docker daemon not reachable: %w", err)
		}
	})
	return dockerProbeErr
}

// dockerRuns reports, once per process, whether the daemon can start
// and finish a container within dockerRunProbeBudget. Only meaningful
// after dockerAvailable succeeded.
func dockerRuns() error {
	dockerRunProbeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), dockerRunProbeBudget)
		defer cancel()
		out, err := execCommand(ctx, "docker", "run", "--rm", dockerRunProbeImage, "true").CombinedOutput()
		if err != nil {
			if ctx.Err() != nil {
				err = fmt.Errorf("no container finished within %s", dockerRunProbeBudget)
			}
			dockerRunProbeErr = fmt.Errorf("docker answers but cannot run a container: %w (output: %s)", err, strings.TrimSpace(string(out)))
		}
	})
	return dockerRunProbeErr
}

// SkipUnlessContainersRun skips t when no Docker daemon is reachable or
// the reachable one cannot start a container (storage stalled, cgroup
// trouble, …). PG_HARDSTORAGE_DEMAND_DOCKER=1 turns either skip into a
// failure, for CI that promises Docker and must not lose a suite
// silently. For tests that start containers through the product itself
// (the verify --full sandbox) rather than through a sink.
func SkipUnlessContainersRun(t testing.TB) {
	t.Helper()
	demand := os.Getenv("PG_HARDSTORAGE_DEMAND_DOCKER") == "1"
	if err := dockerAvailable(); err != nil {
		if demand {
			t.Fatalf("PG_HARDSTORAGE_DEMAND_DOCKER=1 but %v", err)
		}
		t.Skipf("%v; skipping", err)
	}
	if err := dockerRuns(); err != nil {
		if demand {
			t.Fatalf("PG_HARDSTORAGE_DEMAND_DOCKER=1 but %v", err)
		}
		t.Skipf("%v; skipping", err)
	}
}

// UpForTest starts a fresh sink of the given kind for t, exports its
// credentials into t's environment and tears it down when t ends.
//
// It skips t when the suite cannot or should not run here: under
// `go test -short` (an emulator container per sub-case is not a short
// test), without a Docker daemon that can start a container, or when
// the fixture image cannot execute on this platform. PG_HARDSTORAGE_DEMAND_DOCKER=1 turns
// the first two skips into a run / a failure, for CI that promises
// Docker and must not lose the suite silently.
//
// Every start is bounded by testSinkUpBudget, and once a kind has
// failed to start testSinkGiveUpAfter times in a row the remaining
// tests fail immediately.
func UpForTest(t testing.TB, kind string) Runtime {
	t.Helper()
	demand := os.Getenv("PG_HARDSTORAGE_DEMAND_DOCKER") == "1"
	if testing.Short() && !demand {
		t.Skipf("%s sink needs a Docker container; skipped under -short (PG_HARDSTORAGE_DEMAND_DOCKER=1 to run)", kind)
	}
	SkipUnlessContainersRun(t)

	testSinkMu.Lock()
	failures, last := testSinkFailures[kind], testSinkLastErr[kind]
	testSinkMu.Unlock()
	if failures >= testSinkGiveUpAfter {
		t.Fatalf("%s sink failed to start %d times in a row in this run; not waiting again. Last error: %v", kind, failures, last)
	}

	s, err := New(kind)
	if err != nil {
		t.Fatalf("sink.New(%s): %v", kind, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testSinkUpBudget)
	defer cancel()
	if err := s.Up(ctx); err != nil {
		// Whatever Up left behind (a container that never became
		// ready) must not outlive the test.
		_ = s.Down(context.Background())
		// A fixture image built for another architecture exits with
		// "exec format error" at once: an environment limit, not a
		// regression, and not a reason to give up on the kind.
		var ce *ContainerExitError
		if errors.As(err, &ce) && ce.IsPlatformMismatch() {
			t.Skipf("%s fixture image cannot execute on this platform: %v", kind, err)
		}
		if ctx.Err() != nil {
			err = fmt.Errorf("not up within %s: %w", testSinkUpBudget, err)
		}
		testSinkMu.Lock()
		testSinkFailures[kind]++
		testSinkLastErr[kind] = err
		testSinkMu.Unlock()
		t.Fatalf("sink.Up(%s): %v", kind, err)
	}
	testSinkMu.Lock()
	testSinkFailures[kind] = 0
	testSinkMu.Unlock()
	t.Cleanup(func() { _ = s.Down(context.Background()) })
	for k, v := range s.EnvForAgent() {
		t.Setenv(k, v)
	}
	return s
}
