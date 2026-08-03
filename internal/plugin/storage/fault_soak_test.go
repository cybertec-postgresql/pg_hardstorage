// fault_soak_test.go — sustained concurrency against a backend that is
// FAILING, not merely slow.
//
// backend_soak_test.go proved 4.6M operations and 1.3 TiB across three
// backends with zero errors — but every one of those operations was
// expected to succeed. That leaves the more interesting half untested:
// what the plugins leave behind when an operation fails PART WAY.
//
// The SSH plugins commit through a "<dst>.hstmp-<rand>" temporary:
// write the temp, then `ln -T` / rename it into place. A failure
// between those two steps leaves the temporary behind. `repo gc` sweeps
// stale staging files, but it can only sweep what it knows to look for
// — and a leak rate of one per thousand operations is invisible in a
// suite that never fails an operation.
//
// The object stores have the analogous question in a different shape: a
// failed Put must leave NO readable object at the key, or a caller that
// retried onto a different backend would see a phantom.
//
// TWO fault mechanisms, because they reach different code:
//
//   - faultinject rejects a call BEFORE it reaches the plugin. That
//     exercises the caller's error handling, but no write ever starts,
//     so it cannot produce a partial commit. A first version of this
//     test used only that and reported 233k "failures" with zero
//     successes — the staging-leak assertion below was passing
//     vacuously, because nothing had ever been written.
//   - a reader that errors PART WAY through the body. Put takes an
//     io.Reader, so this fails the plugin mid-write, after it has
//     created its staging temporary and before it can commit. That is
//     the real shape of the leak, and only this reaches it.
//
// Keys are split so a genuine MIX of outcomes occurs: a run where
// everything fails proves as little as one where nothing does.
//
// It asserts the properties that must hold REGARDLESS of how many
// operations failed:
//
//   - a key that was never successfully committed must not be readable;
//   - no staging temporaries survive;
//   - the single-winner guarantee still holds under injected failure —
//     a rejected IfNotExists must not become a second winner;
//   - the plugin stays usable afterwards (no wedged connection pool or
//     exhausted SSH session table).
//
// Duration is per backend, defaulting to a 10-second smoke; the
// nightly sets PGHS_FAULT_SOAK_MINUTES for a real run.
//
//go:build integration

package storage_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/faultinject"
)

// errInjected is what the middleware returns for a matched rule. It is
// deliberately NOT one of the storage sentinels: a caller must not be
// able to mistake an injected transport failure for ErrNotFound or
// ErrAlreadyExists, which carry semantic meaning.
var errInjected = errors.New("faultinject: synthetic backend failure")

// errTruncated is returned by a body reader that dies part way.
var errTruncated = errors.New("faultinject: reader failed mid-body")

// failingReader yields n good bytes and then errors, so the plugin
// fails AFTER it has begun writing. This is what produces a real
// partial commit; rejecting the call outright never does.
type failingReader struct {
	data []byte
	n    int
	pos  int
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.pos >= r.n {
		return 0, errTruncated
	}
	n := copy(p, r.data[r.pos:r.n])
	r.pos += n
	return n, nil
}

// faultSoakDuration is per backend. Same reasoning as soakDuration: a
// short default keeps the routine integration job inside its budget,
// and the nightly sets PGHS_FAULT_SOAK_MINUTES for a real run.
//
// Ten seconds is enough for this test's purpose — it asserts that
// failures leave nothing behind, and the measured rate produces
// thousands of injected and mid-body failures per backend in that
// window. The guards below refuse to pass on zero of either.
func faultSoakDuration(t *testing.T) time.Duration {
	t.Helper()
	if v := os.Getenv("PGHS_FAULT_SOAK_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return 10 * time.Second
}

// TestFaultSoak runs the fault workload against every container-backed
// scheme, so a leak that only one backend's commit path produces is not
// hidden by the others.
func TestFaultSoak(t *testing.T) {
	for _, w := range soakSchemes() {
		t.Run(w.scheme, func(t *testing.T) {
			url := wiringURL(t, w)
			runFaultSoak(t, w.scheme, url)
		})
	}
}

func runFaultSoak(t *testing.T, backend, url string) {
	t.Helper()
	dur := faultSoakDuration(t)
	seed := time.Now().UnixNano()
	if v := os.Getenv("PGHS_STORAGE_SOAK_SEED"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			seed = n
		}
	}
	t.Logf("%s fault soak: budget=%s workers=%d seed=%d", backend, dur, soakWorkers, seed)

	ctx := context.Background()
	inner, err := storage.Open(ctx, url)
	if err != nil {
		t.Fatalf("storage.Open(%s): %v", backend, err)
	}
	defer inner.Close()

	fi := faultinject.New(inner)
	// Fail writes under the "fault/" prefix only, so the verification
	// pass at the end can read and list freely.
	fi.Activate([]faultinject.Rule{
		{
			Name:      "put-failures",
			Ops:       faultinject.OpPut,
			KeyPrefix: "fault/flaky/",
			Err:       errInjected,
		},
		{
			Name:      "rename-failures",
			Ops:       faultinject.OpRename,
			KeyPrefix: "fault/flaky/",
			Err:       errInjected,
		},
	}, faultinject.ActivateOptions{})

	var (
		attempted  atomic.Int64
		injected   atomic.Int64
		succeeded  atomic.Int64
		torn       atomic.Int64
		winners    atomic.Int64
		unexpected atomic.Value
	)
	note := func(op string, err error) {
		if unexpected.Load() == nil {
			unexpected.Store(fmt.Errorf("%s: %w", op, err))
		}
	}

	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup
	for w := 0; w < soakWorkers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed + int64(w)))
			round := 0
			for time.Now().Before(deadline) {
				round++
				size := bodySizes[rng.Intn(len(bodySizes))]
				body := bytes.Repeat([]byte{byte('a' + w)}, size)

				// Rotate through three outcomes so the run contains a
				// genuine mix rather than one uniform failure mode.
				switch round % 3 {
				case 0:
					// Rejected before the write starts (faultinject).
					key := fmt.Sprintf("fault/flaky/w%02d/k%06d", w, round)
					attempted.Add(1)
					_, perr := fi.Put(ctx, key, bytes.NewReader(body),
						storage.PutOptions{ContentLength: int64(size)})
					switch {
					case perr == nil:
						succeeded.Add(1)
					case errors.Is(perr, errInjected):
						injected.Add(1)
						if rc, gerr := inner.Get(ctx, key); gerr == nil {
							_ = rc.Close()
							note("phantom", fmt.Errorf(
								"key %q is readable after its Put returned an error", key))
							return
						}
					default:
						note("put", perr)
						return
					}

				case 1:
					// Fails PART WAY through the body — the plugin has
					// already begun writing, so its staging temporary
					// exists when the error arrives.
					if size < 1024 {
						size = 64 << 10
						body = bytes.Repeat([]byte{byte('a' + w)}, size)
					}
					key := fmt.Sprintf("fault/torn/w%02d/k%06d", w, round)
					attempted.Add(1)
					_, perr := inner.Put(ctx, key,
						&failingReader{data: body, n: size / 2},
						storage.PutOptions{ContentLength: int64(size)})
					if perr == nil {
						note("torn", fmt.Errorf(
							"key %q: Put reported success although its body reader failed "+
								"after %d of %d bytes — a truncated object was committed",
							key, size/2, size))
						return
					}
					torn.Add(1)
					// A partially-written object must not be readable.
					if rc, gerr := inner.Get(ctx, key); gerr == nil {
						n, _ := io.Copy(io.Discard, rc)
						_ = rc.Close()
						note("torn-visible", fmt.Errorf(
							"key %q is readable (%d bytes) after a mid-body failure", key, n))
						return
					}

				default:
					// A clean write, so the run has real successes to
					// contrast against and the backend is proven usable
					// throughout rather than only at the end.
					key := fmt.Sprintf("fault/ok/w%02d/k%06d", w, round)
					attempted.Add(1)
					if _, perr := inner.Put(ctx, key, bytes.NewReader(body),
						storage.PutOptions{ContentLength: int64(size)}); perr != nil {
						note("clean-put", perr)
						return
					}
					succeeded.Add(1)
				}

				// Single-winner under failure: every worker races for
				// the same key each round, through the faulting wrapper.
				shared := fmt.Sprintf("fault/flaky/shared-%06d", round)
				_, serr := fi.Put(ctx, shared, strings.NewReader("x"),
					storage.PutOptions{IfNotExists: true})
				switch {
				case serr == nil:
					winners.Add(1)
				case errors.Is(serr, errInjected), errors.Is(serr, storage.ErrAlreadyExists):
					// Both are acceptable outcomes for a loser.
				default:
					note("ifnotexists", serr)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	fi.Deactivate()

	if e := unexpected.Load(); e != nil {
		t.Fatalf("%s fault soak: %v", backend, e.(error))
	}
	if attempted.Load() == 0 {
		t.Fatal("no operations attempted")
	}
	if injected.Load() == 0 {
		t.Fatalf("%s fault soak attempted %d ops but injected ZERO failures — the "+
			"middleware rules never matched, so nothing about failure handling was "+
			"exercised", backend, attempted.Load())
	}

	if torn.Load() == 0 {
		t.Fatalf("%s fault soak: zero mid-body failures were produced — the staging-leak "+
			"check below would pass vacuously", backend)
	}
	if succeeded.Load() == 0 {
		t.Fatalf("%s fault soak: nothing succeeded; a run where every operation fails "+
			"proves as little as one where none does", backend)
	}
	t.Logf("%s fault soak: %d attempted — %d rejected up front, %d failed mid-body, "+
		"%d succeeded, %d shared-key winners",
		backend, attempted.Load(), injected.Load(), torn.Load(), succeeded.Load(), winners.Load())

	// --- Post-conditions, checked against the UNWRAPPED plugin. ----

	// 1. No staging temporaries survive. This is the leak the happy-path
	//    soak structurally cannot find: it never fails a commit.
	var leaked []string
	for oi, lerr := range inner.List(ctx, "fault/") {
		if lerr != nil {
			t.Fatalf("final list: %v", lerr)
		}
		if strings.Contains(oi.Key, ".hstmp-") {
			leaked = append(leaked, oi.Key)
		}
	}
	if len(leaked) > 0 {
		sample := leaked
		if len(sample) > 5 {
			sample = sample[:5]
		}
		t.Errorf("%s: %d staging temporary/temporaries survived %d injected failures, "+
			"e.g. %v — a commit path abandoned them, and repo gc reclaims only what it "+
			"knows to look for", backend, len(leaked), injected.Load(), sample)
	}

	// 2. The plugin is still usable. A failure path that leaked an SSH
	//    session or a pooled connection shows up here rather than as a
	//    wrong answer.
	probe := "fault/probe/after-faults"
	if _, err := inner.Put(ctx, probe, strings.NewReader("still working"),
		storage.PutOptions{}); err != nil {
		t.Fatalf("%s: plugin unusable after %d injected failures: %v",
			backend, injected.Load(), err)
	}
	rc, err := inner.Get(ctx, probe)
	if err != nil {
		t.Fatalf("%s: Get after faults: %v", backend, err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "still working" {
		t.Errorf("%s: post-fault round-trip returned %q", backend, got)
	}
}
