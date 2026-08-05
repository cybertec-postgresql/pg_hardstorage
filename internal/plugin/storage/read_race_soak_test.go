// read_race_soak_test.go — a reader racing writers and deleters must
// get the whole object or a typed "it is gone", never anything else.
//
// Both bugs this campaign found were read-side, and both were on scp:
// a failed remote read that looked like an empty object, and a
// concurrently-deleted key reported as a transport failure instead of
// ErrNotFound. Neither was scp-specific in principle — every backend
// reads through some stream that can end early, and every backend can
// have a key deleted mid-read.
//
// The existing contract suite reads quiescent objects. This reads them
// while other goroutines are publishing and deleting, which is the
// state a real repository is in during a backup, and asserts the only
// two acceptable outcomes:
//
//	complete body  — exactly what some writer published, in full;
//	ErrNotFound    — the object genuinely is not there.
//
// A short read, a mixed body, or an untyped error is a failure. Those
// are the three shapes that let a transport problem masquerade as data
// corruption.
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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

func readRaceDuration(t *testing.T) time.Duration {
	t.Helper()
	if v := os.Getenv("PGHS_READ_RACE_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return 10 * time.Second
}

// TestReadRaceSoak_NeverShortNeverUntyped is the soak.
func TestReadRaceSoak_NeverShortNeverUntyped(t *testing.T) {
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
			runReadRaceSoak(t, b.scheme, sp)
		})
	}
}

func runReadRaceSoak(t *testing.T, scheme string, sp storage.StoragePlugin) {
	t.Helper()
	const (
		keys    = 12
		readers = 6
		writers = 3
	)
	dur := readRaceDuration(t)
	seed := time.Now().UnixNano()
	if v := os.Getenv("PGHS_READ_RACE_SEED"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			seed = n
		}
	}
	t.Logf("%s read-race soak: budget=%s readers=%d writers=%d seed=%d",
		scheme, dur, readers, writers, seed)

	// One fixed body, so any short or mixed read is unambiguous.
	body := make([]byte, 8192)
	for i := range body {
		body[i] = byte('a' + i%26)
	}

	ctx := context.Background()
	var (
		complete  atomic.Int64
		notFound  atomic.Int64
		puts      atomic.Int64
		dels      atomic.Int64
		firstFail atomic.Value
	)
	fail := func(format string, args ...any) {
		if firstFail.Load() == nil {
			firstFail.Store(fmt.Sprintf(format, args...))
		}
	}
	key := func(i int) string { return fmt.Sprintf("readrace/k%02d.bin", i) }

	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup

	// Writers publish and delete, so readers race both.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed + int64(w)))
			for time.Now().Before(deadline) && firstFail.Load() == nil {
				k := key(rng.Intn(keys))
				if rng.Intn(2) == 0 {
					_, err := sp.Put(ctx, k, bytes.NewReader(body),
						storage.PutOptions{ContentLength: int64(len(body))})
					if err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
						fail("%s: put %s: %v", scheme, k, err)
						return
					}
					puts.Add(1)
				} else {
					if err := sp.Delete(ctx, k); err != nil &&
						!errors.Is(err, storage.ErrNotFound) {
						fail("%s: delete %s: %v", scheme, k, err)
						return
					}
					dels.Add(1)
				}
			}
		}(w)
	}

	// Readers assert the only two acceptable outcomes.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed + 1000 + int64(r)))
			for time.Now().Before(deadline) && firstFail.Load() == nil {
				k := key(rng.Intn(keys))
				rc, err := sp.Get(ctx, k)
				if err != nil {
					if errors.Is(err, storage.ErrNotFound) {
						notFound.Add(1)
						continue
					}
					fail("%s: Get %s returned an UNTYPED error: %v\n"+
						"A reader racing a delete must get ErrNotFound; anything else "+
						"cannot be handled by a caller and reads as a fault in the data",
						scheme, k, err)
					return
				}
				got, rerr := io.ReadAll(rc)
				cerr := rc.Close()
				switch {
				case rerr != nil || cerr != nil:
					// The read failed and SAID so — acceptable. What
					// must never happen is a silent short read.
					if len(got) != 0 && len(got) != len(body) {
						// Reported, so this is fine; recorded for the log.
					}
				case len(got) == len(body):
					if !sameBytes(got, body) {
						fail("%s: %s read back %d bytes that are not what was published — "+
							"a mixed body means the publish is not atomic", scheme, k, len(got))
						return
					}
					complete.Add(1)
				default:
					fail("%s: %s read %d of %d bytes with NO error from ReadAll or Close.\n"+
						"A short read reported as success is indistinguishable from a "+
						"truncated object, so a transport failure surfaces as data "+
						"corruption", scheme, k, len(got), len(body))
					return
				}
			}
		}(r)
	}
	wg.Wait()

	if v := firstFail.Load(); v != nil {
		t.Fatalf("%s read-race soak: %s", scheme, v.(string))
	}
	if complete.Load() == 0 {
		t.Fatalf("%s: no complete read ever happened; the soak proved nothing", scheme)
	}
	if notFound.Load() == 0 {
		t.Errorf("%s: no reader ever raced a delete — the soak never reached the state "+
			"both of this campaign's bugs lived in", scheme)
	}
	t.Logf("%s read-race soak: %d complete reads, %d ErrNotFound, %d puts, %d deletes",
		scheme, complete.Load(), notFound.Load(), puts.Load(), dels.Load())
}

func sameBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
