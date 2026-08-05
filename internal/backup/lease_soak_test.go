// lease_soak_test.go — randomised lease lifecycles against real
// backends, checking the one invariant that matters.
//
// The lease was rebuilt twice recently: an atomic break claim closed
// the reclaim window, and acquisition now refuses backends that cannot
// enforce it. Both are covered by tests that stage ONE interleaving
// each. The property they protect — at most one holder, ever — is a
// statement about all interleavings, and a scheduler explores shapes a
// hand-written test does not.
//
// So this runs randomised acquire / renew / release / abandon cycles
// from many goroutines and asserts, continuously, that the number of
// live holders never exceeds one. Every operation is chosen by a seeded
// RNG, so a failure is reproducible via PGHS_LEASE_SOAK_SEED.
//
//go:build integration

package backup

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

func leaseSoakDuration(t *testing.T) time.Duration {
	t.Helper()
	if v := os.Getenv("PGHS_LEASE_SOAK_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return 10 * time.Second
}

// TestLeaseSoak_NeverTwoHolders is the soak.
func TestLeaseSoak_NeverTwoHolders(t *testing.T) {
	for _, b := range []struct{ scheme, sinkKind string }{
		{"s3", "s3-minio"},
		{"gcs", "gcs-fake"},
		{"azblob", "azurite"},
		{"scp", "ssh-exec"},
	} {
		t.Run(b.scheme, func(t *testing.T) {
			rt, err := sink.New(b.sinkKind)
			if err != nil {
				t.Fatalf("sink.New: %v", err)
			}
			if err := rt.Up(context.Background()); err != nil {
				t.Fatalf("up: %v", err)
			}
			t.Cleanup(func() { _ = rt.Down(context.Background()) })
			for k, v := range rt.EnvForAgent() {
				t.Setenv(k, v)
			}
			sp, err := storage.Open(context.Background(), rt.URL())
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			t.Cleanup(func() { _ = sp.Close() })
			if !sp.Capabilities().ConditionalPut {
				t.Skipf("%s cannot enforce a lease here", b.scheme)
			}
			runLeaseSoak(t, b.scheme, sp)
		})
	}
}

func runLeaseSoak(t *testing.T, scheme string, sp storage.StoragePlugin) {
	t.Helper()
	const contenders = 6
	dur := leaseSoakDuration(t)
	seed := time.Now().UnixNano()
	if v := os.Getenv("PGHS_LEASE_SOAK_SEED"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			seed = n
		}
	}
	t.Logf("%s lease soak: budget=%s contenders=%d seed=%d", scheme, dur, contenders, seed)

	ctx := context.Background()
	var (
		// live counts holders that believe they hold RIGHT NOW. It must
		// never exceed 1 — that is the whole guarantee.
		live      atomic.Int64
		maxLive   atomic.Int64
		acquires  atomic.Int64
		blocked   atomic.Int64
		renews    atomic.Int64
		lost      atomic.Int64
		abandoned atomic.Int64
		firstFail atomic.Value
	)
	fail := func(format string, args ...any) {
		if firstFail.Load() == nil {
			firstFail.Store(fmt.Sprintf(format, args...))
		}
	}

	// A short TTL so abandonment (a simulated crash) becomes
	// reclaimable inside the soak rather than after it.
	const ttl = 2 * time.Second

	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup
	for c := 0; c < contenders; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed + int64(c)))
			owner := fmt.Sprintf("soak-%02d", c)

			for time.Now().Before(deadline) && firstFail.Load() == nil {
				l, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{
					Owner: owner, TTL: ttl,
				})
				switch {
				case err == nil:
					acquires.Add(1)
				case errors.Is(err, ErrBackupInProgress):
					blocked.Add(1)
					time.Sleep(time.Duration(rng.Intn(20)) * time.Millisecond)
					continue
				default:
					fail("%s: acquire returned an unexpected error: %v", scheme, err)
					return
				}

				// We believe we hold it. Nobody else may.
				n := live.Add(1)
				for {
					m := maxLive.Load()
					if n <= m || maxLive.CompareAndSwap(m, n) {
						break
					}
				}
				if n > 1 {
					fail("%s: %d holders at once — mutual exclusion violated. Reproduce "+
						"with PGHS_LEASE_SOAK_SEED=%d", scheme, n, seed)
					live.Add(-1)
					return
				}

				// Hold for a random slice of the TTL, renewing sometimes.
				hold := time.Duration(rng.Intn(int(ttl/time.Millisecond))) * time.Millisecond
				endHold := time.Now().Add(hold)
				for time.Now().Before(endHold) {
					time.Sleep(20 * time.Millisecond)
					if rng.Intn(3) != 0 {
						continue
					}
					if rerr := l.Renew(ctx); rerr != nil {
						if errors.Is(rerr, ErrLeaseLost) {
							lost.Add(1)
							// We no longer hold it; drop the count and
							// stop claiming to.
							live.Add(-1)
							goto next
						}
						fail("%s: renew failed unexpectedly: %v", scheme, rerr)
						live.Add(-1)
						return
					}
					renews.Add(1)
				}

				// Either release cleanly or ABANDON — a crashed holder,
				// which is what the break-claim path exists for.
				if rng.Intn(4) == 0 {
					abandoned.Add(1)
					live.Add(-1)
					// Deliberately no Release: let it lapse.
					time.Sleep(ttl)
				} else {
					live.Add(-1)
					if rerr := l.Release(ctx); rerr != nil {
						fail("%s: release failed: %v", scheme, rerr)
						return
					}
				}
			next:
			}
		}(c)
	}
	wg.Wait()

	if v := firstFail.Load(); v != nil {
		t.Fatalf("%s lease soak: %s", scheme, v.(string))
	}
	if acquires.Load() == 0 {
		t.Fatalf("%s: nobody ever acquired the lease; the soak proved nothing", scheme)
	}
	if blocked.Load() == 0 {
		t.Errorf("%s: no contender was ever blocked — the soak never contended, so the "+
			"exclusion it claims to test was never exercised", scheme)
	}
	if maxLive.Load() > 1 {
		t.Fatalf("%s: peak concurrent holders = %d", scheme, maxLive.Load())
	}
	t.Logf("%s lease soak: %d acquires, %d blocked, %d renews, %d lost, %d abandoned, "+
		"peak holders %d", scheme, acquires.Load(), blocked.Load(), renews.Load(),
		lost.Load(), abandoned.Load(), maxLive.Load())
}
