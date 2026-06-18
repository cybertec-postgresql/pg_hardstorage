package fs_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	stdio "io"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// TestPutExclusive_ConcurrentReadNeverPartial pins the fix for the
// non-atomic IfNotExists Put race: a writer creating a key with
// O_CREATE|O_EXCL and THEN writing the body left a window where the key
// existed but was empty, so a racing reader could read 0 bytes (and, in
// the backup lease, fail with "unexpected end of JSON input"). The
// atomic staging-write + link(2) makes the key appear only once its
// content is complete.
//
// Each iteration races one writer against several readers spinning on
// the same key; every successful read MUST return the complete body.
func TestPutExclusive_ConcurrentReadNeverPartial(t *testing.T) {
	p := openFS(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte("lease-payload-0123456789-"), 40) // ~1 KiB

	const iterations = 300
	const readers = 6
	for it := 0; it < iterations; it++ {
		key := fmt.Sprintf("leases/db/k-%d.json", it)
		var wg sync.WaitGroup

		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.Put(ctx, key, bytes.NewReader(body), storage.PutOptions{
				ContentLength: int64(len(body)),
				IfNotExists:   true,
			})
		}()

		wg.Add(readers)
		for r := 0; r < readers; r++ {
			go func() {
				defer wg.Done()
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					rc, err := p.Get(ctx, key)
					if err != nil {
						if errors.Is(err, storage.ErrNotFound) {
							continue // not linked into place yet — keep spinning
						}
						t.Errorf("get %s: %v", key, err)
						return
					}
					got, rerr := stdio.ReadAll(rc)
					_ = rc.Close()
					if rerr != nil {
						t.Errorf("read %s: %v", key, rerr)
						return
					}
					if !bytes.Equal(got, body) {
						t.Errorf("PARTIAL READ on %s: got %d bytes, want %d", key, len(got), len(body))
					}
					return
				}
			}()
		}
		wg.Wait()
	}
}

// TestPutExclusive_ConcurrentWritersExactlyOneWins confirms the atomic
// path still enforces IfNotExists: many writers racing the same key
// yield exactly one success, the rest ErrAlreadyExists, and the stored
// body is intact.
func TestPutExclusive_ConcurrentWritersExactlyOneWins(t *testing.T) {
	p := openFS(t)
	ctx := context.Background()
	const writers = 16
	for it := 0; it < 50; it++ {
		key := fmt.Sprintf("locks/k-%d", it)
		body := []byte(fmt.Sprintf("owner-payload-%d", it))
		var wins, exists int64
		var mu sync.Mutex
		var wg sync.WaitGroup
		wg.Add(writers)
		for w := 0; w < writers; w++ {
			go func() {
				defer wg.Done()
				_, err := p.Put(ctx, key, bytes.NewReader(body), storage.PutOptions{
					ContentLength: int64(len(body)),
					IfNotExists:   true,
				})
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					wins++
				case errors.Is(err, storage.ErrAlreadyExists):
					exists++
				default:
					t.Errorf("unexpected put error: %v", err)
				}
			}()
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("iter %d: exactly one writer should win; got %d", it, wins)
		}
		if exists != writers-1 {
			t.Errorf("iter %d: losers = %d, want %d", it, exists, writers-1)
		}
		rc, err := p.Get(ctx, key)
		if err != nil {
			t.Fatalf("get after race: %v", err)
		}
		got, _ := stdio.ReadAll(rc)
		_ = rc.Close()
		if !bytes.Equal(got, body) {
			t.Errorf("stored body corrupted after race: %q", got)
		}
	}
}
