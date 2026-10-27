package streaming_test

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/streaming"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/leakcheck"
)

// Replication reconnect loops create a Reader per connection. Create and
// tear down thousands (clean close, cancel mid-Receive, premature EOF,
// per-Receive ctx cancel) and prove no watcher goroutine, AfterFunc
// registration or net.Conn survives.
func TestLeak_ReaderLifecycle(t *testing.T) {
	n := 200
	if os.Getenv("PGHS_SCALE") == "1" {
		n = 15000
	}
	base := leakcheck.Take()
	leakcheck.Plateau(t, n, n/16, 4<<20, func(i int) {
		ctx, cancel := context.WithCancel(context.Background())
		clientConn, serverConn := net.Pipe()
		r := streaming.NewWithConn(ctx, clientConn, streaming.Options{InactivityTimeout: -1})
		be := pgproto3.NewBackend(serverConn, serverConn)
		switch i % 4 {
		case 0: // normal message then clean close
			go func() { be.Send(&pgproto3.CopyData{Data: []byte("x")}); _ = be.Flush() }()
			if _, err := r.Receive(ctx); err != nil {
				t.Errorf("receive: %v", err)
			}
		case 1: // cancel while blocked in Receive
			go func() { time.Sleep(time.Millisecond); cancel() }()
			_, _ = r.Receive(ctx)
		case 2: // server vanishes
			_ = serverConn.Close()
			_, _ = r.Receive(ctx)
		case 3: // per-receive ctx cancelled, parent alive
			rc, rcancel := context.WithTimeout(ctx, time.Millisecond)
			_, _ = r.Receive(rc)
			rcancel()
		}
		_ = r.Close()
		_ = serverConn.Close()
		cancel()
	})
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{})
}
