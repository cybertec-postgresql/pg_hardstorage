//go:build linux

package fs

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Bug 60 (barrier_linux.go): if the FINAL syncfs (after publishDeferred)
// fails, the already-published entries had been dequeued and were not
// requeued — so a retried Barrier found nothing outstanding, ran NO
// syncfs, and returned nil. The caller was told the data was durable
// even though the directory entries never flushed.
//
// We inject a failure into the second syncfs call via the syncfsFn
// seam. After healing, a retried Barrier must actually run syncfs
// again (i.e. must NOT be a no-op) and succeed.
func TestBarrier_FinalSyncfsFailureRetries(t *testing.T) {
	orig := syncfsFn
	defer func() { syncfsFn = orig }()

	calls := 0
	failSecond := true
	syncfsFn = func(fd int) error {
		calls++
		// Fail the 2nd syncfs (the post-publish flush) on the first
		// Barrier only.
		if failSecond && calls == 2 {
			return errors.New("injected syncfs failure")
		}
		return orig(fd)
	}

	p := openTestPlugin(t)
	ctx := context.Background()
	body := []byte("final-syncfs-durable")
	if _, err := p.PutBytes(ctx, "chunks/f", body, storage.PutOptions{
		IfNotExists: true,
		Durability:  storage.DurabilityDeferred,
	}); err != nil {
		t.Fatalf("deferred put: %v", err)
	}

	// First Barrier: publish succeeds, final syncfs fails -> error.
	if err := p.Barrier(ctx); err == nil {
		t.Fatal("expected Barrier to fail on the injected final-syncfs error")
	}
	callsAfterFirst := calls

	// Retry on a healthy filesystem. This must NOT be a no-op: the
	// entry must still be queued so a real syncfs runs.
	failSecond = false
	if err := p.Barrier(ctx); err != nil {
		t.Fatalf("retry Barrier: %v", err)
	}
	if calls <= callsAfterFirst {
		t.Fatalf("retry Barrier ran no syncfs (barrier was a silent no-op): calls before=%d after=%d",
			callsAfterFirst, calls)
	}

	// And the chunk is readable.
	if got := getBytes(t, p, "chunks/f"); string(got) != string(body) {
		t.Errorf("after retry: got %q, want %q", got, body)
	}
}
