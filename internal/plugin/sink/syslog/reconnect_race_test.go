// reconnect_race_test.go — regression test for the close-under-writer
// and deadline-stomping races in the syslog sink (concurrency audit,
// bug B).
//
// The Dispatcher fans Emits out concurrently with no per-sink
// serialization. Before the fix, write() snapshotted s.conn under the
// mutex but called SetWriteDeadline + Write after unlocking, and a
// failed Emit's dial() unconditionally closed the current connection.
// Interleaving: Emit-1 sits between deadline-set and Write on conn c1;
// Emit-2 fails, dials, and closes c1 mid-use — Emit-1 then writes on a
// closed connection and both events are lost (the dispatcher discards
// sink errors). Concurrent emits also stomped each other's deadline.
//
// This test injects a scripted dialer + fake conns and runs many
// concurrent Emits under -race. On the old code the fake conn records
// writes-after-close (and -race flags the unsynchronized accesses);
// on the fixed code writes are serialized under s.mu and reconnection
// is generation-aware, so exactly one reconnect happens for the one
// failed connection generation.

package syslog

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

var errInjectedWrite = errors.New("fakeconn: injected write failure")

// fakeConn is an in-memory net.Conn that succeeds the first failAfter
// writes and fails every write after that (failAfter < 0: never fail).
// It records Close and counts any Write / SetWriteDeadline attempted
// after Close — the close-under-writer violation the old code trips.
type fakeConn struct {
	failAfter int // number of writes that succeed; <0 = unlimited

	mu         sync.Mutex
	writes     int // successful writes
	attempts   int // all writes (successful + injected failures)
	closed     bool
	afterClose int // Write/SetWriteDeadline calls observed after Close
}

func (c *fakeConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		c.afterClose++
		return 0, errors.New("fakeconn: write on closed conn")
	}
	c.attempts++
	if c.failAfter >= 0 && c.attempts > c.failAfter {
		return 0, errInjectedWrite
	}
	c.writes++
	return len(b), nil
}

func (c *fakeConn) SetWriteDeadline(time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		c.afterClose++
		return errors.New("fakeconn: deadline on closed conn")
	}
	return nil
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *fakeConn) Read([]byte) (int, error)        { return 0, errors.New("fakeconn: not readable") }
func (c *fakeConn) LocalAddr() net.Addr             { return &net.TCPAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr            { return &net.TCPAddr{} }
func (c *fakeConn) SetDeadline(time.Time) error     { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error { return nil }

func (c *fakeConn) stats() (writes, afterClose int, closed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes, c.afterClose, c.closed
}

func TestSyslog_ConcurrentEmitReconnectRace(t *testing.T) {
	const (
		conn1Successes = 3  // conn #1 fails every write after this many
		emitters       = 64 // concurrent Emits
	)

	sink, err := NewFromSpec(output.SinkSpec{Name: "s", Plugin: "syslog", Config: map[string]any{
		"protocol":     "tcp",
		"address":      "127.0.0.1:9", // never actually dialed: dialConn seam below
		"min_severity": "info",
		"timeout":      "2s",
	}})
	if err != nil {
		t.Fatal(err)
	}
	s := sink.(*Sink)

	conn1 := &fakeConn{failAfter: conn1Successes}
	conn2 := &fakeConn{failAfter: -1} // healthy replacement

	var dialMu sync.Mutex
	dials := 0
	s.dialConn = func(context.Context) (net.Conn, error) {
		dialMu.Lock()
		defer dialMu.Unlock()
		dials++
		if dials == 1 {
			return conn1, nil
		}
		return conn2, nil
	}

	ctx := context.Background()
	if err := s.Open(ctx, nil); err != nil {
		t.Fatal(err)
	}

	// Fan out concurrent Emits, like the dispatcher does.
	var wg sync.WaitGroup
	errs := make([]error, emitters)
	for i := 0; i < emitters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.Emit(ctx, output.NewEvent(output.SeverityInfo, "test", "reconnect-race"))
		}(i)
	}
	wg.Wait()

	// Every event must land: failures on conn #1 reconnect and retry
	// on conn #2 (the dispatcher would silently drop returned errors).
	for i, err := range errs {
		if err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}

	w1, after1, closed1 := conn1.stats()
	w2, after2, closed2 := conn2.stats()

	// (a) No write (or deadline-set) is ever attempted on a closed
	// conn. The old unlocked write path trips this: a concurrent
	// dial() closed conn #1 while another Emit was mid-write on it.
	if after1 != 0 {
		t.Errorf("conn #1 saw %d write/deadline ops after Close", after1)
	}
	if after2 != 0 {
		t.Errorf("conn #2 saw %d write/deadline ops after Close", after2)
	}

	// (b) At most one reconnect for the one failed generation: the
	// initial Open dial plus exactly one redial, no matter how many
	// concurrent Emits failed on conn #1.
	if dials != 2 {
		t.Errorf("dialer called %d times, want 2 (Open + one generation-aware reconnect)", dials)
	}
	if !closed1 {
		t.Error("conn #1 was never closed by the reconnect")
	}
	if closed2 {
		t.Error("conn #2 was closed while the sink is still open")
	}

	// Accounting: each Emit wrote its frame exactly once.
	if w1+w2 != emitters {
		t.Errorf("successful writes: conn1=%d + conn2=%d = %d, want %d", w1, w2, w1+w2, emitters)
	}
	if w1 != conn1Successes {
		t.Errorf("conn #1 successful writes = %d, want %d", w1, conn1Successes)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, closed := conn2.stats(); !closed {
		t.Error("conn #2 not closed by sink Close")
	}
}
