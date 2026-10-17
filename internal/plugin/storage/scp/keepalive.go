package scp

// keepalive.go — a dead SSH connection must become an ERROR, not a
// hang, and the handle must heal afterwards.
//
// Port of the sftp plugin's keepalive + reconnect (sftp/keepalive.go,
// sftp/reconnect.go); see there for the soak history. In short:
// x/crypto/ssh sets no deadlines after the dial, so a peer that goes
// silently away (NAT expiry, power loss, partition) leaves every
// session open and every command blocked forever — a `wal fetch`
// inside restore_command hanging recovery, an archiver stalling while
// pg_wal fills. An SSH-level keepalive request on a ticker detects
// the dead peer and closes the transport, which unblocks everything
// parked on it; the next operation then re-dials (rate-limited)
// instead of failing on the dead client until process restart.

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// startKeepalive probes conn until stop closes, and closes conn after
// keepaliveMisses consecutive failed probes, then calls onDead.
func startKeepalive(conn *ssh.Client, stop <-chan struct{}, onDead func()) {
	// Snapshot the tuning on the caller's goroutine so tests restoring
	// the package variables do not race the prober.
	interval, timeout, missBudget := keepaliveInterval, keepaliveTimeout, keepaliveMisses
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		misses := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			// SendRequest blocks on the very connection it probes, so a
			// timer decides the verdict; the stalled probe goroutine is
			// released by the eventual conn.Close.
			done := make(chan error, 1)
			go func() {
				_, _, err := conn.SendRequest("keepalive@openssh.com", true, nil)
				done <- err
			}()
			select {
			case err := <-done:
				if err != nil {
					misses++
				} else {
					misses = 0
				}
			case <-time.After(timeout):
				misses++
			case <-stop:
				return
			}
			if misses >= missBudget {
				_ = conn.Close()
				if onDead != nil {
					onDead()
				}
				return
			}
		}
	}()
}

// dialLocked establishes (or re-establishes) the SSH connection and
// arms its keepalive. Caller holds p.mu.
func (p *Plugin) dialLocked() error {
	// Dial TCP ourselves so kernel keepalives are armed underneath the
	// SSH-level probe.
	raw, err := net.DialTimeout("tcp", p.host, p.cfgssh.Timeout)
	if err != nil {
		return fmt.Errorf("scp: dial %s: %w", p.host, err)
	}
	if tc, ok := raw.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}
	sc, chans, reqs, err := ssh.NewClientConn(raw, p.host, p.cfgssh)
	if err != nil {
		_ = raw.Close()
		return fmt.Errorf("scp: dial %s: %w", p.host, err)
	}
	conn := ssh.NewClient(sc, chans, reqs)
	if p.kaStop != nil {
		close(p.kaStop) // retire the previous generation's prober
	}
	if p.ssh != nil {
		_ = p.ssh.Close()
	}
	p.ssh = conn
	p.dead = false
	p.gen++
	p.kaStop = make(chan struct{})
	gen := p.gen
	startKeepalive(conn, p.kaStop, func() { p.markDead(gen) })
	return nil
}

// markDead records that generation gen's connection is gone. A stale
// callback from an older generation is ignored.
func (p *Plugin) markDead(gen uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gen == gen {
		p.dead = true
	}
}

// transportTearDown closes the SSH transport of generation gen and
// marks it dead. This is the read-guard's unblock lever: a stdout Read
// parked in x/crypto/ssh's buffered channel reader is only released by
// closing the transport (marking it dead alone would not wake it). A
// stale call (gen no longer current) is a no-op so a late timeout from
// an old generation never tears down a fresh re-dial.
func (p *Plugin) transportTearDown(gen uint64) {
	p.mu.Lock()
	kaStop := p.kaStop
	if p.closed || p.gen != gen {
		p.mu.Unlock()
		return
	}
	p.kaStop = nil
	if p.ssh != nil {
		_ = p.ssh.Close()
		p.ssh = nil
	}
	p.dead = true
	p.mu.Unlock()
	// Retire the prober off the lock (its own teardown takes p.mu).
	if kaStop != nil {
		close(kaStop)
	}
}

// conn hands back the live client, re-dialling (rate-limited) after
// the connection was found dead. It also returns the generation so a
// caller that finds the transport broken can mark exactly it dead.
func (p *Plugin) conn() (*ssh.Client, uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.cfgssh == nil {
		return nil, 0, errors.New("scp: plugin not open")
	}
	if p.ssh != nil && !p.dead {
		return p.ssh, p.gen, nil
	}
	if since := time.Since(p.lastRedial); since < redialMinInterval {
		return nil, 0, fmt.Errorf("scp: connection lost %s ago; next reconnect attempt in %s",
			since.Round(time.Second), (redialMinInterval - since).Round(time.Second))
	}
	p.lastRedial = time.Now()
	if err := p.dialLocked(); err != nil {
		return nil, 0, fmt.Errorf("scp: reconnect: %w", err)
	}
	return p.ssh, p.gen, nil
}

// isTransportGone reports whether err from opening a session means the
// SSH transport itself is dead (closed by the keepalive, the peer, or
// a reset) rather than a per-channel refusal.
func isTransportGone(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "broken pipe")
}
