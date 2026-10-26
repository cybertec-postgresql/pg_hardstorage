package scp

// stress_test.go — the read-guard against a REAL in-process SSH server, for
// the scp (ssh-exec) backend.
//
// The exec backend streams a Get over an exec session's `cat <path>` stdout.
// The stress wedge (inproc_server_test.go's wedgeCat) runs a real local cat,
// serves only the first chunk of a wedged file's stdout, then parks — the
// data channel makes no progress while the SSH control channel and keepalive
// stay live. That is the live-peer-wedged-stdout shape the Sept-30
// chaos-storage-soak hang (io.Copy → channel.Read → Cond.Wait, ~1h) had, and
// the one the pre-fix suites never exercised end to end (every earlier repro
// was a DEAD peer, which the keepalive alone catches).

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// TestStressSCP_WedgedStdout_LivePeer: a Get's `cat` stdout that delivers the
// first chunk then parks must surface a stall error within readIdleTimeout
// and tear down the transport of that generation, not hang.
func TestStressSCP_WedgedStdout_LivePeer(t *testing.T) {
	srv := startExecServer(t)
	p := openExecPlugin(t, srv.addr, srv.hostPub)
	old := readIdleTimeout
	readIdleTimeout = 300 * time.Millisecond
	t.Cleanup(func() { readIdleTimeout = old })

	body := bytes.Repeat([]byte("scp-wedged-bytes"), 4096) // 65536 bytes
	if _, err := p.Put(context.Background(), "wedged", bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put wedged: %v", err)
	}

	// The peer goes live-wedged: keepalives keep being answered (control
	// channel untouched), the cat stdout serves the first half of the file
	// then parks with no more bytes and no EOF. Only the read guard can
	// bound the drain — the keepalive stays quiet.
	srv.setWedge("wedged", len(body)/2)

	genBefore := p.genNow()
	rc, gerr := p.Get(context.Background(), "wedged")
	if gerr != nil {
		t.Fatalf("Get open: %v", gerr)
	}
	var got int
	start := time.Now()
	_, rerr := io.Copy(io.Discard, &scpCountingReader{rc: rc, n: &got})
	_ = rc.Close()
	elapsed := time.Since(start)
	if rerr == nil {
		t.Fatalf("read over a wedged cat stdout succeeded — the read guard is not " +
			"bounding the no-progress drain")
	}
	if !errors.Is(rerr, errSCPReadStalled) {
		t.Fatalf("wedged stdout surfaced %q; want the stall error (the data channel "+
			"must be bounded by the read guard, not the keepalive)", rerr)
	}
	if got < 16384 {
		t.Fatalf("wedged stdout delivered only %d bytes before stalling; the first "+
			"chunk must arrive before the park", got)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("wedged stdout took %v; the guard fires at %v (bounded, not hung)",
			elapsed, readIdleTimeout)
	}
	if p.genNow() == genBefore {
		t.Fatalf("wedged stdout did not tear down its generation (gen still %d) — the "+
			"guard must close the transport so the next operation re-dials", genBefore)
	}
	t.Logf("live peer, wedged cat stdout → stall error after %v (%d bytes delivered), "+
		"generation %d torn down and re-dialled to %d", elapsed, got, genBefore, p.genNow())
}

// TestStressSCP_WedgedStdout_ReconnectsAndHeals: after the wedged read tears
// the transport down, the handle must heal — a fresh Get on a HEALTHY key
// reconnects (the exec server is still up) and succeeds.
func TestStressSCP_WedgedStdout_ReconnectsAndHeals(t *testing.T) {
	srv := startExecServer(t)
	p := openExecPlugin(t, srv.addr, srv.hostPub)
	old := readIdleTimeout
	readIdleTimeout = 300 * time.Millisecond
	t.Cleanup(func() { readIdleTimeout = old })
	orl := redialMinInterval
	redialMinInterval = 100 * time.Millisecond
	t.Cleanup(func() { redialMinInterval = orl })

	body := bytes.Repeat([]byte("heal-me"), 4096) // 40960 bytes
	if _, err := p.Put(context.Background(), "wedged", bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put wedged: %v", err)
	}
	if _, err := p.Put(context.Background(), "healthy", strings.NewReader("ok"),
		storage.PutOptions{ContentLength: 2}); err != nil {
		t.Fatalf("put healthy: %v", err)
	}

	srv.setWedge("wedged", len(body))

	rc, gerr := p.Get(context.Background(), "wedged")
	if gerr == nil {
		_, rerr := io.Copy(io.Discard, rc)
		_ = rc.Close()
		if rerr == nil {
			t.Fatalf("wedged Get succeeded — the read guard must bound it")
		}
	}

	// The network (and the exec server) is still up. The handle must heal:
	// a fresh op reconnects and reads the healthy key.
	deadline := time.Now().Add(5 * time.Second)
	for {
		rc, gerr := p.Get(context.Background(), "healthy")
		if gerr == nil {
			b, rerr := io.ReadAll(rc)
			_ = rc.Close()
			if rerr == nil && string(b) == "ok" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("handle never healed after the wedged Get: %v", gerr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestStressSCP_ConcurrentStorm_NoFalsePositive: concurrent Gets of a HEALTHY
// file must never false-positive the read guard. No stall, full bytes, the
// generation stays alive.
func TestStressSCP_ConcurrentStorm_NoFalsePositive(t *testing.T) {
	srv := startExecServer(t)
	p := openExecPlugin(t, srv.addr, srv.hostPub)
	old := readIdleTimeout
	readIdleTimeout = 300 * time.Millisecond
	t.Cleanup(func() { readIdleTimeout = old })
	oi, ot, om := keepaliveInterval, keepaliveTimeout, keepaliveMisses
	keepaliveInterval, keepaliveTimeout, keepaliveMisses = 40*time.Millisecond, 30*time.Millisecond, 3
	t.Cleanup(func() { keepaliveInterval, keepaliveTimeout, keepaliveMisses = oi, ot, om })

	body := bytes.Repeat([]byte("scp-storm-bytes"), 4096) // 69632 bytes
	if _, err := p.Put(context.Background(), "big", bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put: %v", err)
	}

	const N = 8
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			rc, gerr := p.Get(context.Background(), "big")
			if gerr != nil {
				errs <- gerr
				return
			}
			buf, rerr := io.ReadAll(rc)
			_ = rc.Close()
			if rerr != nil {
				errs <- rerr
				return
			}
			if !bytes.Equal(buf, body) {
				errs <- errors.New("byte mismatch under concurrent storm")
				return
			}
			errs <- nil
		}()
	}
	for i := 0; i < N; i++ {
		if e := <-errs; e != nil {
			t.Fatalf("healthy concurrent scp Get #%d failed: %v — the read guard "+
				"false-positived on a live peer", i, e)
		}
	}
	if p.genDead() {
		t.Fatalf("healthy concurrent storm tore down a live generation (false positive)")
	}
	t.Logf("scp concurrent storm: %d parallel 68KiB Gets, zero stall, generation alive", N)
}

// scpCountingReader counts delivered bytes and surfaces the underlying error.
type scpCountingReader struct {
	rc io.Reader
	n  *int
}

func (c *scpCountingReader) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	*c.n += n
	return n, err
}

// genNow returns the current connection generation (mu-guarded). A wedged
// read that tore its transport down and re-dialled leaves a HIGHER
// generation than before — the proof the guard fired, even though the
// handle already self-healed (so p.dead is false again).
func (p *Plugin) genNow() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gen
}
