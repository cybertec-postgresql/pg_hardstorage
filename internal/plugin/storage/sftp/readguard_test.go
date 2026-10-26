package sftp

// readguard_test.go — the read-guard, reproduced deterministically.
//
// The Sept-30 campaign's data-plane stalls (goroutine dumps parked in
// ssh.(*buffer).Read / recvPacket → sync.Cond.Wait) were a LIVE peer whose
// control channel still answered keepalives while one data-channel read
// made no progress. The keepalive cannot see that (its probes succeed), so
// without a per-read bound the operation hangs forever — a `wal fetch`
// inside restore_command hanging PostgreSQL recovery. readguard.go closes
// the transport after readIdleTimeout of no progress, the only lever that
// wakes a read parked in ssh's buffered reader.

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// parkingReader never returns from Read — the exact shape of a read parked
// in ssh's buffered packet reader waiting on a reply that never comes.
// Close is a no-op so a test can release it without a live transport.
type parkingReader struct{}

func (parkingReader) Read([]byte) (int, error) { select {} }
func (parkingReader) Close() error             { return nil }

// testPlugin builds a minimal, never-dialed Plugin at a known generation so
// only the read-guard machinery (not a real connection) is under test.
func testPlugin(t *testing.T) *Plugin {
	t.Helper()
	return &Plugin{gen: 1, kaStop: make(chan struct{})}
}

// closingReader adapts an io.Reader to io.ReadCloser with a no-op Close,
// standing in for an open file that has live data (the *sftp.File only
// needs Read + Close, which are the guardedRead surface).
type closingReader struct{ r io.Reader }

func (c closingReader) Read(p []byte) (int, error) { return c.r.Read(p) }
func (closingReader) Close() error                 { return nil }

// genDead reports whether the current generation was marked dead.
func (p *Plugin) genDead() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dead
}

// TestReadGuard_StalledReadErrorsInsteadOfHanging: a data read that makes
// no progress must return a stall error within a bounded time and tear down
// the transport, instead of hanging. This is the finding.
func TestReadGuard_StalledReadErrorsInsteadOfHanging(t *testing.T) {
	oi := readIdleTimeout
	readIdleTimeout = 150 * time.Millisecond
	t.Cleanup(func() { readIdleTimeout = oi })

	p := testPlugin(t)
	gr := p.guardRead(context.Background(), parkingReader{}, p.gen, readIdleTimeout)
	defer gr.Close()

	done := make(chan error, 1)
	go func() {
		_, err := gr.Read(make([]byte, 4096))
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, errSFTPReadStalled) {
			t.Fatalf("stalled read surfaced %v, want errSFTPReadStalled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled read still blocked 5s after the idle window expired — the parked " +
			"read can only return if the transport was torn down. This is the campaign's " +
			"10-14m buffer.Read stall: a wal fetch inside restore_command hangs recovery.")
	}

	if !p.genDead() {
		t.Fatal("generation not marked dead after a stalled read — the next op would retry " +
			"the wedged transport instead of reconnecting")
	}
}

// TestReadGuard_LiveReadUntouched: a healthy read (immediate data + EOF)
// must pass through untouched with no false-positive teardown. A
// false-positive stall on a live peer would break every long sftp backup.
func TestReadGuard_LiveReadUntouched(t *testing.T) {
	oi := readIdleTimeout
	readIdleTimeout = 150 * time.Millisecond
	t.Cleanup(func() { readIdleTimeout = oi })

	p := testPlugin(t)
	gr := p.guardRead(context.Background(), closingReader{r: strings.NewReader("alive")}, p.gen, readIdleTimeout)
	defer gr.Close()

	buf, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("healthy read errored: %v — the guard must never tear down a live peer", err)
	}
	if string(buf) != "alive" {
		t.Fatalf("healthy read returned %q, want %q", buf, "alive")
	}
	if p.genDead() {
		t.Fatal("healthy read triggered a false-positive transport teardown")
	}
}

// TestReadGuard_ContextCancelTearsDown: a cancelled context on a parked
// read must tear down the transport (unblocking the parked read) and mark
// the generation dead.
func TestReadGuard_ContextCancelTearsDown(t *testing.T) {
	oi := readIdleTimeout
	readIdleTimeout = 150 * time.Millisecond
	t.Cleanup(func() { readIdleTimeout = oi })

	p := testPlugin(t)
	ctx, cancel := context.WithCancel(context.Background())
	gr := p.guardRead(ctx, parkingReader{}, p.gen, readIdleTimeout)
	defer gr.Close()

	go func() {
		<-time.After(30 * time.Millisecond)
		cancel()
	}()

	// The parked Read returns once the cancel hook tears the transport
	// down (idle window is the backstop). Either way the generation must
	// end up dead so the next operation reconnects.
	deadline := time.Now().Add(5 * time.Second)
	for !p.genDead() {
		if time.Now().After(deadline) {
			t.Fatal("context cancel did not tear down the parked read's transport within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestReadGuard_IdealRoundTripBounds: a single read round-trip (a Stat, a
// List Step) that never returns must be bounded by the idle window and
// surfaced as a stall error.
func TestReadGuard_IdealRoundTripBounds(t *testing.T) {
	oi := readIdleTimeout
	readIdleTimeout = 150 * time.Millisecond
	t.Cleanup(func() { readIdleTimeout = oi })

	p := testPlugin(t)

	start := time.Now()
	err := p.idleRoundTrip(p.gen, func() error {
		select {} // the round-trip's response never arrives.
	})
	elapsed := time.Since(start)
	if !errors.Is(err, errSFTPReadStalled) {
		t.Fatalf("wedged round-trip surfaced %v, want errSFTPReadStalled", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("wedged round-trip took %v; the idle window should have bounded it to ~150ms", elapsed)
	}
}

// TestReadGuard_StaleTeardownLeavesCurrentGen: a teardown for a generation
// that is no longer current (a re-dial already replaced it) must be a
// no-op — it must not mark the current generation dead or replace its
// keepalive stop channel.
func TestReadGuard_StaleTeardownLeavesCurrentGen(t *testing.T) {
	p := testPlugin(t)
	p.mu.Lock()
	p.gen = 2 // a re-dial moved us to generation 2.
	p.dead = false
	currentKastop := p.kaStop
	p.mu.Unlock()

	// A late timeout from the OLD generation 1 arrives.
	p.idleTeardown(1)

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

// Anchors: the public surface the campaign's code path uses is unchanged.
var _ io.ReadCloser = (*guardedRead)(nil)
var _ = storage.ErrNotFound
