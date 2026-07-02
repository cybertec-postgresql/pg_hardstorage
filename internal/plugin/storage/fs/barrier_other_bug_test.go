//go:build !linux

package fs

import (
	"context"
	"os"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Bug 1 (barrier_other.go step 1): on a step-1 fsync error at index i,
// Barrier requeued only list[i:], permanently dropping the already-
// fsynced-but-unpublished entries list[:i]. A retried Barrier then
// returned nil while those chunks' final keys never appeared —
// a committed manifest would reference chunks that don't exist.
//
// We provoke a step-1 fsync error at a MIDDLE entry by removing read
// permission from its staging temp (fsyncFile's Open then fails with
// EACCES; ErrNotExist would be treated as success). After healing and
// retrying, EVERY chunk — including those staged before the failure —
// must be published.
func TestBarrier_FsyncErrorMidListLosesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permission checks")
	}
	p := openTestPlugin(t)
	ctx := context.Background()

	keys := []string{"chunks/a", "chunks/b", "chunks/c", "chunks/d"}
	for _, k := range keys {
		if _, err := p.PutBytes(ctx, k, []byte(k), storage.PutOptions{
			IfNotExists: true,
			Durability:  storage.DurabilityDeferred,
		}); err != nil {
			t.Fatalf("deferred put %q: %v", k, err)
		}
	}

	p.mu.Lock()
	victim := p.deferred[2].staging // chunks/c
	p.mu.Unlock()

	if err := os.Chmod(victim, 0o000); err != nil {
		t.Fatalf("chmod victim: %v", err)
	}
	if err := p.Barrier(ctx); err == nil {
		t.Fatal("expected Barrier to fail on the locked staging temp")
	}
	if err := os.Chmod(victim, 0o600); err != nil {
		t.Fatalf("restore victim perms: %v", err)
	}
	if err := p.Barrier(ctx); err != nil {
		t.Fatalf("retry Barrier: %v", err)
	}
	for _, k := range keys {
		if got := getBytes(t, p, k); string(got) != k {
			t.Errorf("chunk %q not durable after retry: got %q", k, got)
		}
	}
	if n := countStagingDeferred(t, p); n != 0 {
		t.Errorf("expected 0 staging temps after successful retry, got %d", n)
	}
}
