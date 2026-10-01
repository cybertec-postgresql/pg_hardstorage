package scp

// readguard_test.go — the SCP read guard, reproduced deterministically.
//
// The Sept-30 campaign's chaos-storage-soak hang (TestFaultSoak, ~1h,
// goroutine dump parked in io.Copy → channel.Read → sync.Cond.Wait) was a
// LIVE peer whose control channel still answered keepalives while the Get's
// `cat` stdout made no progress. The keepalive cannot see that (its probes
// succeed), so without a per-read bound the read hangs forever.
// guardedStdoutRead closes the SSH transport after readIdleTimeout of no
// progress — the only lever that wakes a read parked in x/crypto/ssh's
// buffered channel reader — and marks the generation dead so the next
// operation reconnects.

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// parkingReader never returns from Read — the shape of a stdout Read parked
// in the channel's buffered reader waiting on data that never comes.
type parkingReader struct{}

func (parkingReader) Read([]byte) (int, error) { select {} }

// newTestPlugin builds a minimal, never-dialed scp Plugin at a known
// generation so only the read-guard machinery is under test.
func newTestPlugin() *Plugin {
	return &Plugin{gen: 1, kaStop: make(chan struct{})}
}

func (p *Plugin) genDead() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dead
}

// TestSCPReadGuard_StalledStdoutErrorsInsteadOfHanging: a Get's stdout read
// that makes no progress must surface a stall error within a bounded time
// and tear down the transport, instead of hanging. This is the finding.
func TestSCPReadGuard_StalledStdoutErrorsInsteadOfHanging(t *testing.T) {
	oi := readIdleTimeout
	readIdleTimeout = 150 * time.Millisecond
	t.Cleanup(func() { readIdleTimeout = oi })

	p := newTestPlugin()
	sr := &sessionReader{
		stdout: parkingReader{},
		ctx:    context.Background(),
		p:      p,
		gen:    p.gen,
		idle:   readIdleTimeout,
	}

	start := time.Now()
	n, err := sr.guardedStdoutRead(make([]byte, 4096))
	elapsed := time.Since(start)
	if n != 0 {
		t.Fatalf("stalled read returned %d bytes, want 0", n)
	}
	if !errors.Is(err, errSCPReadStalled) {
		t.Fatalf("stalled stdout read surfaced %v, want errSCPReadStalled", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("stalled stdout read took %v; the idle window should have bounded it to ~150ms", elapsed)
	}
	t.Logf("stalled stdout read bounded after %v: %v", elapsed, err)

	if !p.genDead() {
		t.Fatal("transport teardown did not mark the generation dead — the next Get would " +
			"retry the same wedged transport instead of reconnecting")
	}
}

// TestSCPReadGuard_LiveStdoutUntouched: a healthy stdout read (immediate
// data then EOF) must pass through untouched with no false-positive teardown.
func TestSCPReadGuard_LiveStdoutUntouched(t *testing.T) {
	oi := readIdleTimeout
	readIdleTimeout = 150 * time.Millisecond
	t.Cleanup(func() { readIdleTimeout = oi })

	p := newTestPlugin()
	sr := &sessionReader{
		stdout: strings.NewReader("alive"),
		ctx:    context.Background(),
		p:      p,
		gen:    p.gen,
		idle:   readIdleTimeout,
	}

	var buf []byte
	for {
		tmp := make([]byte, 64)
		n, err := sr.guardedStdoutRead(tmp)
		buf = append(buf, tmp[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("healthy stdout read errored: %v — the guard must never tear down a live peer", err)
		}
	}
	if string(buf) != "alive" {
		t.Fatalf("healthy stdout read returned %q, want %q", buf, "alive")
	}
	if p.genDead() {
		t.Fatal("healthy stdout read triggered a false-positive transport teardown")
	}
}

// TestSCPReadGuard_StaleTeardownLeavesCurrentGen: a teardown for a
// generation that is no longer current (a re-dial already replaced it) must
// be a no-op — it must not mark the current generation dead or replace its
// keepalive stop channel.
func TestSCPReadGuard_StaleTeardownLeavesCurrentGen(t *testing.T) {
	p := newTestPlugin()
	p.mu.Lock()
	p.gen = 2 // a re-dial moved us to generation 2.
	p.dead = false
	currentKastop := p.kaStop
	p.mu.Unlock()

	// A late timeout from the OLD generation 1 arrives.
	p.transportTearDown(1)

	p.mu.Lock()
	dead := p.dead
	kaAlive := p.kaStop == currentKastop
	p.mu.Unlock()
	if dead {
		t.Fatal("stale teardown marked the CURRENT generation dead")
	}
	if !kaAlive {
		t.Fatal("stale teardown replaced the current generation's keepalive stop channel")
	}
}
