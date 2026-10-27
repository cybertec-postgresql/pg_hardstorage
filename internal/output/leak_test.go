package output_test

import (
	"context"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/leakcheck"
)

type leakDiscardRenderer struct{}

func (leakDiscardRenderer) Name() string                                 { return "discard" }
func (leakDiscardRenderer) SupportsTTY() bool                            { return false }
func (leakDiscardRenderer) Close() error                                 { return nil }
func (leakDiscardRenderer) RenderResult(io.Writer, *output.Result) error { return nil }
func (leakDiscardRenderer) RenderEvent(io.Writer, *output.Event) error   { return nil }

type leakStuckSink struct {
	block chan struct{}
	n     atomic.Int64
}

func (*leakStuckSink) Name() string                               { return "slow" }
func (*leakStuckSink) Open(context.Context, map[string]any) error { return nil }
func (s *leakStuckSink) Emit(ctx context.Context, _ *output.Event) error {
	s.n.Add(1)
	select {
	case <-s.block:
	case <-ctx.Done():
	}
	return nil
}
func (*leakStuckSink) Close() error { return nil }

// A long-lived agent emits events for months. With a sink that is stuck
// (webhook endpoint black-holed), the per-sink queue must stay bounded
// (events are dropped and counted, never accumulated), the foreground
// caller never blocks, and Close must release the delivery goroutine.
func TestLeak_DispatcherStuckSinkStaysBounded(t *testing.T) {
	n := 200000
	if os.Getenv("PGHS_SCALE") == "1" {
		n = 5000000
	}
	base := leakcheck.Take()
	sink := &leakStuckSink{block: make(chan struct{})}
	d := output.NewDispatcher(leakDiscardRenderer{}, io.Discard, io.Discard)
	d.DrainTimeout = 100 * time.Millisecond
	d.AddSink(sink)
	ctx := context.Background()
	leakcheck.Plateau(t, n, n/16, 8<<20, func(i int) {
		ev := output.NewEvent(output.SeverityNotice, "agent", "tick").WithBody(map[string]any{"i": i, "pad": "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"})
		_ = d.Event(ctx, ev)
	})
	_ = d.Close()
	close(sink.block)
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{})
}
