package walsink

import (
	"context"
	"testing"
)

// TestChunkSegment_ReturnsBufferOnCancelledContext pins resource-cleanup
// audit #1: chunkSegment's cancelled-procCtx early return must hand the
// borrowed 16 MiB segment buffer back to the pool, like every other exit
// from chunkSegment does. Leaving it out leaks a pooled buffer.
func TestChunkSegment_ReturnsBufferOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Sink{
		bufPool: make(chan []byte, 2),
		procCtx: ctx,
	}
	// Pool starts full (capacity 2). Borrow one — simulating the receive
	// side holding a filled buffer it's about to hand to the processor.
	s.bufPool <- make([]byte, 8)
	s.bufPool <- make([]byte, 8)
	buf := <-s.bufPool
	if len(s.bufPool) != 1 {
		t.Fatalf("setup: pool holds %d buffers, want 1 after one borrow", len(s.bufPool))
	}

	// Cancel the processor context — the only trigger for the early return.
	cancel()

	if _, err := s.chunkSegment(&segJob{buf: buf, n: 0}); err == nil {
		t.Fatal("chunkSegment must error on a cancelled procCtx")
	}

	if len(s.bufPool) != 2 {
		t.Fatalf("buffer not returned to the pool: pool holds %d, want 2 (resource-cleanup audit #1)", len(s.bufPool))
	}
}
