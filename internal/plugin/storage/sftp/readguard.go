package sftp

// readguard.go — a LIVE peer whose data channel wedges must become an
// ERROR, not a hang.
//
// The keepalive (keepalive.go) covers a DEAD peer: its control-channel
// probes fail, it closes the transport, and every parked operation
// unblocks. It does NOT cover the other failure the Sept-30 release
// campaign hit — a peer whose CONTROL channel is still alive and answers
// keepalives while one DATA-channel READ makes no progress. That is the
// shape of the campaign's 10-14 minute stalls (goroutine dumps in
// ssh.(*buffer).Read / recvPacket, a `wal fetch` inside restore_command
// hanging PostgreSQL recovery indefinitely).
//
// The stall is in x/crypto/ssh's BUFFERED packet reader, which waits on a
// sync.Cond — not the socket. The peer's keepalive replies keep the
// socket actively receiving, so a TCP read deadline would never fire:
// nothing unblocks a parked buffer.Read except closing the transport
// (the same lever the keepalive uses). readIdleTimeout bounds a data-plane
// read that makes no progress; on timeout the transport is closed (this
// unblocks the parked read) and the generation is marked dead so the next
// operation re-dials (reconnect.go). A healthy read returns in
// milliseconds, so a no-progress window of readIdleTimeout is unambiguously
// wedged.
//
// The guard is a no-op when readIdleTimeout is 0.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/pkg/sftp"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

var errSFTPReadStalled = errors.New("sftp: read stalled: no progress for readIdleTimeout; tore down the connection, the next operation reconnects")

// connGen returns the live client and its generation under the lock,
// plus the current idle window. The generation is what the read-guard
// teardown compares against, so it kills only the connection it is
// reading on, never a fresher re-dial.
func (p *Plugin) connGen() (*sftp.Client, uint64, time.Duration, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.cfgssh == nil {
		return nil, 0, 0, errClosed
	}
	if p.client != nil && !p.dead {
		return p.client, p.gen, readIdleTimeout, nil
	}
	if since := time.Since(p.lastRedial); since < redialMinInterval {
		return nil, 0, 0, fmt.Errorf("sftp: connection lost %s ago; next reconnect attempt in %s",
			since.Round(time.Second), (redialMinInterval - since).Round(time.Second))
	}
	p.lastRedial = time.Now()
	if err := p.dialLocked(); err != nil {
		return nil, 0, 0, fmt.Errorf("sftp: reconnect: %w", err)
	}
	return p.client, p.gen, readIdleTimeout, nil
}

// idleTeardown closes the SSH transport of generation gen (the only
// lever that unblocks a read parked in ssh's buffered reader) and marks
// that generation dead. A stale call (gen no longer current) is a no-op.
func (p *Plugin) idleTeardown(gen uint64) {
	p.mu.Lock()
	if p.closed || p.gen != gen {
		// Stale: a newer generation owns the connection now. The old
		// transport it read on was already retired by the re-dial, so
		// there is nothing to tear down — touching the current
		// generation's stop channel here would be a bug.
		p.mu.Unlock()
		return
	}
	kaStop := p.kaStop
	p.kaStop = nil
	// TRANSPORT FIRST, exactly as keepalive.go: sftp.Client.Close writes
	// on the same connection it is closing, so closing the client first
	// can itself block. Closing the SSH client unblocks both.
	if p.ssh != nil {
		_ = p.ssh.Close()
	}
	if p.client != nil {
		_ = p.client.Close()
		p.client = nil
	}
	p.dead = true
	p.mu.Unlock()
	// Retire the prober off the lock (its own teardown takes p.mu, so
	// closing under the lock would deadlock with the keepalive).
	if kaStop != nil {
		close(kaStop)
	}
}

// guardedRead wraps an open sftp file so its data-channel reads are
// bounded by the no-progress idle window. A read that returns (data,
// EOF, or a genuine error) is passed through untouched; a read that
// blocks for the whole window with no progress tears down the transport
// and reports a stall, so the caller's retry/refusal machinery sees an
// error instead of a hang. A cancelled context tears the transport down
// the same way, surfacing the context error.
type guardedRead struct {
	// f is the open file (an *sftp.File in production) or, in tests, any
	// io.ReadCloser standing in for a data channel. The guard only ever
	// calls Read and Close on it.
	f    io.ReadCloser
	p    *Plugin
	gen  uint64
	idle time.Duration

	stopCtx func() bool
	once    sync.Once
}

func (g *guardedRead) Read(p []byte) (int, error) {
	if g.idle <= 0 {
		return g.f.Read(p)
	}
	type res struct {
		n   int
		err error
	}
	done := make(chan res, 1)
	go func() {
		n, err := g.f.Read(p)
		done <- res{n, err}
	}()
	select {
	case rr := <-done:
		return rr.n, rr.err
	case <-time.After(g.idle):
		g.once.Do(func() { g.p.idleTeardown(g.gen) })
		return 0, errSFTPReadStalled
	}
}

// Close releases the open file's data channel and detaches the
// context-cancellation hook. It is the io.ReadCloser's single owner of
// the file handle, so the caller never touches the underlying *sftp.File.
func (g *guardedRead) Close() error {
	if g.stopCtx != nil {
		g.stopCtx()
	}
	if g.f != nil {
		return g.f.Close()
	}
	return nil
}

// guardRead wraps an open file in a guardedRead, arming a context hook
// that tears the transport down on cancellation (a read parked in the
// buffered reader never looks at ctx on its own).
func (p *Plugin) guardRead(ctx context.Context, f io.ReadCloser, gen uint64, idle time.Duration) *guardedRead {
	g := &guardedRead{f: f, p: p, gen: gen, idle: idle}
	if ctx != nil && idle > 0 {
		g.stopCtx = context.AfterFunc(ctx, func() { p.idleTeardown(gen) })
	}
	return g
}

// idleRoundTrip runs a single request/response round-trip (a Stat, a
// List's next Step, the dial's NewClient) with the same no-progress
// bound as a Get read. The request runs in its own goroutine (it blocks
// in the buffered reader and cannot be interrupted short of the
// teardown), the timer decides the verdict, and the stalled goroutine
// is released by the eventual conn.Close.
func (p *Plugin) idleRoundTrip(gen uint64, do func() error) error {
	idle := readIdleTimeout
	if idle <= 0 {
		return do()
	}
	done := make(chan error, 1)
	go func() { done <- do() }()
	select {
	case err := <-done:
		return err
	case <-time.After(idle):
		p.idleTeardown(gen)
		return errSFTPReadStalled
	}
}

// guardedOpen runs cli.Open (a single round-trip) under the idle guard
// and, on success, wraps the open file in a guardedRead. The context
// hook is applied by the caller via guardRead (which needs the
// generation). A stall kills the transport and reports the stall
// error; a not-found surfaces as storage.ErrNotFound.
func (p *Plugin) guardedOpen(ctx context.Context, gen uint64, cli *sftp.Client, full string) (*guardedRead, error) {
	idle := readIdleTimeout
	if idle <= 0 {
		f, err := cli.Open(full)
		if err != nil {
			if isNotExist(err) {
				return nil, storage.ErrNotFound
			}
			return nil, fmt.Errorf("sftp: open %s: %w", full, err)
		}
		return p.guardRead(ctx, f, gen, idle), nil
	}
	type res struct {
		f   *sftp.File
		err error
	}
	done := make(chan res, 1)
	go func() {
		f, err := cli.Open(full)
		done <- res{f, err}
	}()
	select {
	case rr := <-done:
		if rr.err != nil {
			if isNotExist(rr.err) {
				return nil, storage.ErrNotFound
			}
			return nil, fmt.Errorf("sftp: open %s: %w", full, rr.err)
		}
		return p.guardRead(ctx, rr.f, gen, idle), nil
	case <-time.After(idle):
		p.idleTeardown(gen)
		return nil, errSFTPReadStalled
	}
}
