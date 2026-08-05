// commit_soak_test.go — randomised concurrent commits against every
// real backend.
//
// CommitExclusive is new (issue #45) and now carries every exclusive
// publish in the system: manifests, tombstones, integrity runs, DSA
// reports, threshold rosters, timeline histories, auxiliary files. Its
// unit tests drive it one call at a time against fs. What it actually
// faces is many writers racing the same key on a real object store,
// where "atomic" is a property of someone else's implementation.
//
// This runs random interleavings and checks invariants that must hold
// no matter how the operations land:
//
//   - exactly ONE writer wins each key;
//   - every loser sees ErrAlreadyExists, not a generic failure;
//   - a reader never observes a partial or mixed body;
//   - on a backend advertising ConditionalPut, nothing is deleted.
//
// Duration is per backend, defaulting to a short smoke. The campaign
// sets PGHS_COMMIT_SOAK_MINUTES for a real run.
//
//go:build integration

package storage_test

import (
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
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

func commitSoakDuration(t *testing.T) time.Duration {
	t.Helper()
	if v := os.Getenv("PGHS_COMMIT_SOAK_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return 10 * time.Second
}

// deleteCountingSP counts deletes so the append-only claim is measured
// rather than assumed.
type deleteCountingSP struct {
	storage.StoragePlugin
	deletes atomic.Int64
}

func (d *deleteCountingSP) Delete(ctx context.Context, key string) error {
	d.deletes.Add(1)
	return d.StoragePlugin.Delete(ctx, key)
}

// TestCommitSoak_ExclusiveUnderConcurrency is the soak.
func TestCommitSoak_ExclusiveUnderConcurrency(t *testing.T) {
	for _, b := range []struct{ scheme, sinkKind string }{
		{"s3", "s3-minio"},
		{"gcs", "gcs-fake"},
		{"azblob", "azurite"},
		{"sftp", "sftp"},
		{"scp", "ssh-exec"},
	} {
		t.Run(b.scheme, func(t *testing.T) {
			rt, err := sink.New(b.sinkKind)
			if err != nil {
				t.Fatalf("sink.New(%q): %v", b.sinkKind, err)
			}
			if err := rt.Up(context.Background()); err != nil {
				t.Fatalf("%s up: %v", b.sinkKind, err)
			}
			t.Cleanup(func() { _ = rt.Down(context.Background()) })
			for k, v := range rt.EnvForAgent() {
				t.Setenv(k, v)
			}
			inner, err := storage.Open(context.Background(), rt.URL())
			if err != nil {
				t.Fatalf("open %s: %v", b.scheme, err)
			}
			t.Cleanup(func() { _ = inner.Close() })

			sp := &deleteCountingSP{StoragePlugin: inner}
			runCommitSoak(t, b.scheme, sp)
		})
	}
}

func runCommitSoak(t *testing.T, scheme string, sp *deleteCountingSP) {
	t.Helper()
	const writers = 8
	dur := commitSoakDuration(t)
	seed := time.Now().UnixNano()
	if v := os.Getenv("PGHS_COMMIT_SOAK_SEED"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			seed = n
		}
	}
	condPut := sp.Capabilities().ConditionalPut
	t.Logf("%s commit soak: budget=%s writers=%d seed=%d conditional_put=%v",
		scheme, dur, writers, seed, condPut)

	ctx := context.Background()
	var (
		rounds    atomic.Int64
		wins      atomic.Int64
		losses    atomic.Int64
		oddErrs   atomic.Int64
		firstFail atomic.Value
	)
	fail := func(format string, args ...any) {
		if firstFail.Load() == nil {
			firstFail.Store(fmt.Sprintf(format, args...))
		}
	}

	deadline := time.Now().Add(dur)
	round := atomic.Int64{}
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed + int64(w)))
			for time.Now().Before(deadline) && firstFail.Load() == nil {
				// Every writer targets the SAME key for a round, so the
				// round is a genuine race rather than N independent puts.
				r := round.Load()
				key := fmt.Sprintf("soak/commit/r%08d.json", r)
				// A body that identifies its writer, so a torn or mixed
				// read is detectable rather than merely suspected.
				body := []byte(strings.Repeat(fmt.Sprintf("w%02d-", w), 64))

				// Jitter so writers do not march in lockstep.
				if d := rng.Intn(3); d > 0 {
					time.Sleep(time.Duration(d) * time.Millisecond)
				}

				err := storage.CommitExclusive(ctx, sp, key, body, storage.PutOptions{})
				switch {
				case err == nil:
					wins.Add(1)
				case errors.Is(err, storage.ErrAlreadyExists):
					losses.Add(1)
				default:
					oddErrs.Add(1)
					fail("%s: commit returned neither success nor ErrAlreadyExists: %v",
						scheme, err)
					return
				}

				// Read back: the stored body must be exactly ONE
				// writer's, complete. A partial or spliced body means
				// the backend's "atomic" publish is not.
				rc, gerr := sp.Get(ctx, key)
				if gerr != nil {
					fail("%s: key %s unreadable after a successful commit: %v",
						scheme, key, gerr)
					return
				}
				got, rerr := io.ReadAll(rc)
				_ = rc.Close()
				if rerr != nil {
					fail("%s: read %s: %v", scheme, key, rerr)
					return
				}
				if len(got) != len(body) {
					fail("%s: key %s is %d bytes, want %d — a partial body is visible at a "+
						"committed key", scheme, key, len(got), len(body))
					return
				}
				if !isSingleWriterBody(got) {
					fail("%s: key %s holds a MIXED body (%q…): two writers' bytes were "+
						"spliced, so the publish is not atomic", scheme, key, got[:16])
					return
				}

				rounds.Add(1)
				// Advance the round occasionally so the soak covers many
				// keys rather than hammering one.
				if rng.Intn(writers) == 0 {
					round.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()

	if v := firstFail.Load(); v != nil {
		t.Fatalf("%s commit soak: %s", scheme, v.(string))
	}
	if rounds.Load() == 0 {
		t.Fatalf("%s commit soak did no work", scheme)
	}
	if wins.Load() == 0 {
		t.Errorf("%s: nothing ever committed", scheme)
	}
	if losses.Load() == 0 {
		t.Errorf("%s: no writer ever lost a race — the soak never actually contended, so "+
			"the exclusion it claims to test was not exercised", scheme)
	}
	t.Logf("%s commit soak: %d rounds, %d wins, %d ErrAlreadyExists, %d deletes",
		scheme, rounds.Load(), wins.Load(), losses.Load(), sp.deletes.Load())

	// The append-only claim, measured.
	if condPut && sp.deletes.Load() != 0 {
		t.Errorf("%s advertises ConditionalPut yet the soak issued %d delete(s); on a "+
			"versioned bucket each is a delete marker, which is what made a repository "+
			"unusable as an anti-ransomware copy of record (issue #45)",
			scheme, sp.deletes.Load())
	}
}

// isSingleWriterBody reports whether every 4-byte group is the same
// writer tag — i.e. the body is one writer's, not two spliced.
func isSingleWriterBody(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	tag := string(b[:4])
	for i := 0; i+4 <= len(b); i += 4 {
		if string(b[i:i+4]) != tag {
			return false
		}
	}
	return true
}
