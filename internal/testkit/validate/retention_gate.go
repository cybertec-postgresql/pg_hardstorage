package validate

import (
	"context"
	"sync"
	"time"
)

// retentionGate lets the fleet's cells run backups and verifies freely
// while letting a retention window hold new ones and wait for the
// in-flight ones to drain — the quiet repository gc needs. Unlike a
// plain RWMutex, the wait is bounded: a cell stuck in a long backup
// makes the window give up rather than stall every other cell behind
// a writer-preferring lock.
type retentionGate struct {
	mu      sync.Mutex
	cond    *sync.Cond
	active  int  // backups/verifies in flight
	pending bool // a window wants the fleet quiet
}

func newRetentionGate() *retentionGate {
	g := &retentionGate{}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// enter blocks while a window is pending or open, then registers one
// in-flight operation. It returns false (without registering) if ctx
// ends first.
func (g *retentionGate) enter(ctx context.Context) bool {
	stop := context.AfterFunc(ctx, g.wake)
	defer stop()
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.pending && ctx.Err() == nil {
		g.cond.Wait()
	}
	if ctx.Err() != nil {
		return false
	}
	g.active++
	return true
}

// leave ends an operation registered by enter.
func (g *retentionGate) leave() {
	g.mu.Lock()
	g.active--
	g.cond.Broadcast()
	g.mu.Unlock()
}

// quiesce holds new operations and waits up to timeout for in-flight
// ones to finish. On success the window is open (resume closes it); on
// timeout or ctx end it is closed again and quiesce returns false.
func (g *retentionGate) quiesce(ctx context.Context, timeout time.Duration) bool {
	// The timer itself decides the timeout: it sets timedOut under the
	// lock and wakes the waiter. Re-reading the clock against a deadline
	// computed after arming the timer let a waiter woken by the timer
	// see "not yet" and wait again with no timer left to wake it — so a
	// busy fleet stalled the window until something else broadcast.
	timedOut := false
	t := time.AfterFunc(timeout, func() {
		g.mu.Lock()
		timedOut = true
		g.cond.Broadcast()
		g.mu.Unlock()
	})
	defer t.Stop()
	stop := context.AfterFunc(ctx, g.wake)
	defer stop()

	g.mu.Lock()
	defer g.mu.Unlock()
	g.pending = true
	for g.active > 0 && ctx.Err() == nil && !timedOut {
		g.cond.Wait()
	}
	if g.active == 0 && ctx.Err() == nil {
		return true
	}
	g.pending = false
	g.cond.Broadcast()
	return false
}

// resume closes a window opened by quiesce.
func (g *retentionGate) resume() {
	g.mu.Lock()
	g.pending = false
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (g *retentionGate) wake() {
	g.mu.Lock()
	g.cond.Broadcast()
	g.mu.Unlock()
}
