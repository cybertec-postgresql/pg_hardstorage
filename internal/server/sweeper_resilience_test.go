package server_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// panicSweepBackend panics on SweepAbandoned; everything else is the
// normal in-memory backend.
type panicSweepBackend struct{ *server.MemoryBackend }

func (panicSweepBackend) SweepAbandoned(context.Context, time.Duration) (int, error) {
	panic("synthetic backend panic")
}

// errSweepBackend fails SweepAbandoned with a fixed error.
type errSweepBackend struct{ *server.MemoryBackend }

func (errSweepBackend) SweepAbandoned(context.Context, time.Duration) (int, error) {
	return 0, errors.New("backend unavailable")
}

// TestRunSweeper_RecoversBackendPanic pins poor-error-handling audit #4:
// a panic in the backend during a sweep tick is recovered into an error
// and surfaced to the tick callback — it must NOT crash the long-lived
// sweeper goroutine (which would take down the whole control plane).
// Against the unrecovered code the goroutine panic aborts the test binary.
func TestRunSweeper_RecoversBackendPanic(t *testing.T) {
	r := server.NewJobRegistryWithBackend(panicSweepBackend{server.NewMemoryBackend()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan error, 8)
	r.RunSweeper(ctx, time.Millisecond, func(_ int, err error) {
		select {
		case got <- err:
		default:
		}
	})

	select {
	case err := <-got:
		if err == nil || !strings.Contains(err.Error(), "panic") {
			t.Errorf("expected a recovered-panic error from the tick; got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no sweeper tick fired — a backend panic must not kill the sweeper")
	}
	cancel()
	r.Stop()
}

// TestRunSweeper_SurfacesBackendError pins the other half of #4: a backend
// error during a sweep is reported to the tick callback rather than being
// silently dropped (`n, _ := ...`).
func TestRunSweeper_SurfacesBackendError(t *testing.T) {
	r := server.NewJobRegistryWithBackend(errSweepBackend{server.NewMemoryBackend()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan error, 8)
	r.RunSweeper(ctx, time.Millisecond, func(_ int, err error) {
		if err != nil {
			select {
			case got <- err:
			default:
			}
		}
	})

	select {
	case err := <-got:
		if !strings.Contains(err.Error(), "backend unavailable") {
			t.Errorf("expected the backend error surfaced to the tick; got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backend sweep error was not surfaced to the tick callback")
	}
	cancel()
	r.Stop()
}

// TestRunSweeper_HealthyTickReportsNoError: the normal path reports a nil
// error (and zero reaped on an idle registry), so the callback can
// distinguish success from failure.
func TestRunSweeper_HealthyTickReportsNoError(t *testing.T) {
	r := server.NewJobRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan error, 8)
	r.RunSweeper(ctx, time.Millisecond, func(_ int, err error) {
		select {
		case got <- err:
		default:
		}
	})

	select {
	case err := <-got:
		if err != nil {
			t.Errorf("healthy sweep tick should report nil error; got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no sweeper tick fired")
	}
	cancel()
	r.Stop()
}
