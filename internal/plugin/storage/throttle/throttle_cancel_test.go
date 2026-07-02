package throttle_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/throttle"
)

// Bug 59: acquire called sleepFn(wait) with no select on ctx.Done, so
// a context cancelled DURING the sleep did not preempt the wait — the
// Put blocked for the full sleep window despite the doc promising
// preemption. This differs from the existing pre-cancel test, which
// the old code passed via the short-circuit BEFORE the sleep.
//
// We inject a sleepFn that blocks until we cancel the context. If the
// sleep is cancellable, Put returns context.Canceled promptly once we
// cancel; if not, it hangs until the (never-signalled) sleep returns
// and the test times out.
func TestThrottle_CancelDuringSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	release := make(chan struct{})
	sleeping := make(chan struct{}, 1)
	blockingSleep := func(time.Duration) {
		// Signal that we entered the sleep, then block until released
		// (which the test never does — it relies on ctx cancellation
		// to unblock acquire instead).
		select {
		case sleeping <- struct{}{}:
		default:
		}
		<-release
	}
	defer close(release)

	// A tiny cap + a body far larger than the burst guarantees a
	// non-trivial wait, so acquire reaches the sleep.
	tr := throttle.New(fsBackend(t), 1024,
		throttle.WithBurst(1024),
		throttle.WithChunkSize(512),
		throttle.WithClock(time.Now, blockingSleep))
	body := bytes.Repeat([]byte{7}, 8*1024)

	errCh := make(chan error, 1)
	go func() {
		_, err := tr.Put(ctx, "k", bytes.NewReader(body),
			storage.PutOptions{ContentLength: int64(len(body))})
		errCh <- err
	}()

	// Wait until acquire is actually blocked in the sleep, then cancel.
	select {
	case <-sleeping:
	case <-time.After(5 * time.Second):
		t.Fatal("throttle never reached the sleep")
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Put after mid-sleep cancel: err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Put did not return after cancellation — sleep was not preempted by ctx")
	}
}
