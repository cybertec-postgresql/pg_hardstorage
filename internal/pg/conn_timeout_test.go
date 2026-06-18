package pg_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

// External-review-pass-5: pg.Connect against an unreachable address
// MUST return within DefaultConnectTimeout, even when the caller
// passes context.Background() (no deadline). Pre-fix:
// pgconn.ConnectConfig with cfg.ConnectTimeout=0 (libpq's default)
// would block indefinitely on certain failure modes (TCP black-hole,
// half-open connections after a partition).
//
// We use a TCP listener bound but never accepting (so connect-time
// hangs in SYN-sent / SYN-received state) to simulate the
// black-hole. The OS-level dial WILL eventually time out via the
// kernel's tcp_syn_retries (typically 60-127 seconds on Linux),
// but DefaultConnectTimeout (30s) is the upper bound we promise.
//
// Skip if the platform's TCP stack happens to fail-fast on the
// listener — we want to exercise the slow path, not the
// connection-refused path.
func TestConnect_DefaultTimeoutBoundsBlackHole(t *testing.T) {
	if testing.Short() {
		t.Skip("connect-timeout test waits up to 30s; -short")
	}
	// Bind a listener but DON'T accept. This makes connect-time
	// behavior depend on the kernel's accept queue. On macOS the
	// queue is shallow; on Linux deeper. Either way: SYN goes
	// unanswered semantically, the connect blocks until the kernel
	// gives up — or until pgconn's ConnectTimeout fires (the path
	// we're testing).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	t.Cleanup(func() { _ = ln.Close() })

	dsn := "postgres://x:x@127.0.0.1:" +
		netPort(addr) + "/db?sslmode=disable"

	// Use Background ctx — no deadline. The fix's whole point is
	// that we no longer rely on the caller's ctx for the
	// connect-time bound.
	start := time.Now()
	_, err = pg.Connect(context.Background(), dsn, pg.ModeRegular)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Connect against a non-PG listener should fail")
	}
	// Some platforms answer the SYN immediately (kernel accepts
	// then drops on first read because we never call accept). In
	// that case the test exercises a different path — pgconn's
	// PG-side handshake fails fast with EOF/RST before our
	// timeout fires. That's still a pass for the operator: they
	// don't see infinite hang.
	//
	// What we actually pin: elapsed < 2*DefaultConnectTimeout.
	// Pre-fix, this could be infinite (or 60-127s kernel bound).
	maxElapsed := 2 * pg.DefaultConnectTimeout
	if elapsed > maxElapsed {
		t.Errorf("Connect took %v, want < %v (pre-fix could hang indefinitely)",
			elapsed, maxElapsed)
	}
}

// netPort extracts the numeric port from a TCPAddr.
func netPort(a *net.TCPAddr) string {
	return strings.TrimPrefix(a.String()[strings.LastIndex(a.String(), ":"):], ":")
}
