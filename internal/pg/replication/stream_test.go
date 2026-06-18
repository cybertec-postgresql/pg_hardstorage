package replication

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/streaming"
)

// recordingSink captures every OnRecord callback. SyncedLSN is
// configurable so tests can verify status-update reporting.
type recordingSink struct {
	mu        sync.Mutex
	records   []XLogRecord
	syncedLSN atomic.Uint64
	onRecErr  error
}

func (s *recordingSink) OnRecord(_ context.Context, r XLogRecord) error {
	s.mu.Lock()
	// Copy WALData so the caller's buffer can be reused.
	cp := make([]byte, len(r.Data))
	copy(cp, r.Data)
	r.Data = cp
	s.records = append(s.records, r)
	s.mu.Unlock()
	return s.onRecErr
}

func (s *recordingSink) SyncedLSN() pglogrepl.LSN {
	return pglogrepl.LSN(s.syncedLSN.Load())
}

// pipeBackends builds a duplex pipe and returns:
//   - reader: a streaming.Reader on the "client" side
//   - sendBackend: the backend half (we encode messages and write
//     them; the reader on the other end consumes them)
//   - clientReadFromServerWriter: the conn end the test holds to
//     decode standby-status updates the receive loop sends back
type pipePair struct {
	reader      *streaming.Reader
	sendBackend *pgproto3.Backend
	serverConn  net.Conn
}

func newPipePair(t *testing.T, ctx context.Context) *pipePair {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	r := streaming.NewWithConn(ctx, clientConn, streaming.Options{
		InactivityTimeout: 5 * time.Second,
	})
	be := pgproto3.NewBackend(serverConn, serverConn)
	t.Cleanup(func() {
		_ = r.Close()
		_ = serverConn.Close()
	})
	return &pipePair{reader: r, sendBackend: be, serverConn: serverConn}
}

// emitCopyData wraps body in a CopyData and flushes through the
// backend. Mirrors what PG would emit during streaming.
func emitCopyData(t *testing.T, be *pgproto3.Backend, body []byte) {
	t.Helper()
	be.Send(&pgproto3.CopyData{Data: body})
	if err := be.Flush(); err != nil {
		t.Errorf("flush: %v", err)
	}
}

// encodeXLogData builds the body of a CopyData for an XLogData message.
// Layout: 'w' + WALStart(BE u64) + ServerWALEnd(BE u64) + ServerTime(BE i64) + WAL data.
func encodeXLogData(walStart, serverEnd uint64, serverTimeMicros int64, walData []byte) []byte {
	out := make([]byte, 0, 25+len(walData))
	out = append(out, pglogrepl.XLogDataByteID)
	out = appendUint64BE(out, walStart)
	out = appendUint64BE(out, serverEnd)
	out = appendUint64BE(out, uint64(serverTimeMicros))
	out = append(out, walData...)
	return out
}

// encodeKeepalive builds the body of a CopyData for a PrimaryKeepalive.
// Layout: 'k' + ServerWALEnd(BE u64) + ServerTime(BE i64) + ReplyRequested(byte).
func encodeKeepalive(serverEnd uint64, serverTimeMicros int64, replyRequested bool) []byte {
	out := make([]byte, 0, 18)
	out = append(out, pglogrepl.PrimaryKeepaliveMessageByteID)
	out = appendUint64BE(out, serverEnd)
	out = appendUint64BE(out, uint64(serverTimeMicros))
	if replyRequested {
		out = append(out, 1)
	} else {
		out = append(out, 0)
	}
	return out
}

func TestRunReceiveLoop_DeliversXLogDataToSink(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pp := newPipePair(t, ctx)
	sink := &recordingSink{}

	// Start draining the server side BEFORE we touch the receive loop:
	// net.Pipe is synchronous and the loop's initial status-update
	// write blocks otherwise.
	go drainServerWrites(pp.serverConn)

	// Server emits one XLogData; the loop processes it; we cancel.
	go func() {
		emitCopyData(t, pp.sendBackend, encodeXLogData(0x1000, 0x2000, 12345, []byte("hello WAL")))
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := runReceiveLoop(ctx, pp.reader, sink, 50*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled; got %v", err)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.records) != 1 {
		t.Fatalf("got %d records, want 1", len(sink.records))
	}
	r := sink.records[0]
	if uint64(r.WALStart) != 0x1000 {
		t.Errorf("WALStart = %x, want 0x1000", uint64(r.WALStart))
	}
	if uint64(r.ServerEnd) != 0x2000 {
		t.Errorf("ServerEnd = %x, want 0x2000", uint64(r.ServerEnd))
	}
	if string(r.Data) != "hello WAL" {
		t.Errorf("Data = %q, want \"hello WAL\"", r.Data)
	}
}

func TestRunReceiveLoop_KeepaliveWithReplyTriggersStatusUpdate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pp := newPipePair(t, ctx)
	sink := &recordingSink{}
	sink.syncedLSN.Store(0xDEADBEEF)

	statusUpdates := make(chan []byte, 16)
	go consumeServerWrites(pp.serverConn, statusUpdates)

	// Server emits a keepalive with reply_requested=1.
	go func() {
		emitCopyData(t, pp.sendBackend, encodeKeepalive(0x3000, 999, true))
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_ = runReceiveLoop(ctx, pp.reader, sink, time.Hour) // no periodic ticks
	count := drainStatusUpdates(statusUpdates, 30*time.Millisecond)
	if count < 2 {
		t.Errorf("expected >=2 status updates (initial + reply); got %d", count)
	}
}

func TestRunReceiveLoop_PeriodicStatusUpdates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pp := newPipePair(t, ctx)
	sink := &recordingSink{}

	statusUpdates := make(chan []byte, 64)
	go consumeServerWrites(pp.serverConn, statusUpdates)

	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	_ = runReceiveLoop(ctx, pp.reader, sink, 30*time.Millisecond)
	count := drainStatusUpdates(statusUpdates, 30*time.Millisecond)
	if count < 3 {
		t.Errorf("expected >=3 status updates; got %d", count)
	}
}

// drainStatusUpdates reads every available 'r' (Standby Status Update)
// byte-0 frame from ch with a short tail-deadline so we count what
// has already arrived without racing channel-close against a still-
// running consumer goroutine.
func drainStatusUpdates(ch <-chan []byte, tail time.Duration) int {
	count := 0
	deadline := time.NewTimer(tail)
	defer deadline.Stop()
	for {
		select {
		case body := <-ch:
			if len(body) >= 1 && body[0] == 'r' {
				count++
			}
		case <-deadline.C:
			return count
		}
	}
}

func TestRunReceiveLoop_SinkErrorAborts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pp := newPipePair(t, ctx)
	go drainServerWrites(pp.serverConn)

	sentinel := errors.New("sink boom")
	sink := &recordingSink{onRecErr: sentinel}

	go func() {
		emitCopyData(t, pp.sendBackend, encodeXLogData(0x1000, 0x2000, 0, []byte("x")))
	}()

	err := runReceiveLoop(ctx, pp.reader, sink, time.Hour)
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sink error to propagate; got %v", err)
	}
}

func TestRunReceiveLoop_ServerErrorMidStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pp := newPipePair(t, ctx)
	go drainServerWrites(pp.serverConn)

	go func() {
		// Tear down with an ErrorResponse before any XLogData.
		pp.sendBackend.Send(&pgproto3.ErrorResponse{
			Severity: "ERROR", Code: "57014", Message: "canceling statement",
		})
		_ = pp.sendBackend.Flush()
	}()

	err := runReceiveLoop(ctx, pp.reader, &recordingSink{}, time.Hour)
	var se *streaming.ServerError
	if !errors.As(err, &se) {
		t.Errorf("expected *streaming.ServerError; got %v", err)
	}
}

func TestRunReceiveLoop_PrematureEOF(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pp := newPipePair(t, ctx)
	go drainServerWrites(pp.serverConn)

	go func() {
		// Close the server side without sending anything.
		time.Sleep(20 * time.Millisecond)
		_ = pp.serverConn.Close()
	}()

	err := runReceiveLoop(ctx, pp.reader, &recordingSink{}, time.Hour)
	if err == nil {
		t.Fatal("expected error on premature EOF")
	}
}

func be64(b []byte) uint64 {
	return uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
}

func TestBuildStatusUpdate_Shape(t *testing.T) {
	// issue #101: write (received) and flush (durable) are reported
	// SEPARATELY — write ahead of flush so a fast PG shutdown can
	// release the walsender while the slot's restart_lsn (flush) stays
	// at the durable point.
	const write = pglogrepl.LSN(0x12345678)
	const flush = pglogrepl.LSN(0x12340000)
	body, err := buildStatusUpdate(write, flush)
	if err != nil {
		t.Fatal(err)
	}
	want := 1 + 8 + 8 + 8 + 8 + 1
	if len(body) != want {
		t.Errorf("length = %d, want %d", len(body), want)
	}
	if body[0] != 'r' {
		t.Errorf("first byte = %x, want 'r'", body[0])
	}
	if got := be64(body[1:9]); got != uint64(write) {
		t.Errorf("write LSN = %x, want %x", got, uint64(write))
	}
	if got := be64(body[9:17]); got != uint64(flush) {
		t.Errorf("flush LSN = %x, want %x", got, uint64(flush))
	}
	if got := be64(body[17:25]); got != uint64(write) {
		t.Errorf("apply LSN = %x, want %x (write)", got, uint64(write))
	}
}

// drainServerWrites reads and discards everything the server end of
// the pipe receives until the pipe is closed. Used by tests that
// don't care about the standby-status replies but need them drained
// to avoid deadlocking the synchronous net.Pipe.
func drainServerWrites(conn net.Conn) {
	buf := make([]byte, 4096)
	for {
		_, err := conn.Read(buf)
		if err != nil {
			return
		}
	}
}

// consumeServerWrites parses CopyData frames the receive loop sends
// back over the pipe and forwards their inner bodies to ch. The
// frontend's role; pgproto3.NewFrontend reads from a connection.
func consumeServerWrites(conn net.Conn, ch chan<- []byte) {
	fr := pgproto3.NewFrontend(conn, conn)
	for {
		msg, err := fr.Receive()
		if err != nil {
			return
		}
		if cd, ok := msg.(*pgproto3.CopyData); ok {
			cp := make([]byte, len(cd.Data))
			copy(cp, cd.Data)
			select {
			case ch <- cp:
			default:
			}
		}
	}
}

// lastStatusUpdate drains 'r' frames and returns the write/flush LSNs
// from the last one seen within tail.
func lastStatusUpdate(ch <-chan []byte, tail time.Duration) (write, flush uint64, ok bool) {
	deadline := time.NewTimer(tail)
	defer deadline.Stop()
	for {
		select {
		case body := <-ch:
			if len(body) >= 17 && body[0] == 'r' {
				write, flush, ok = be64(body[1:9]), be64(body[9:17]), true
			}
		case <-deadline.C:
			return
		}
	}
}

// TestRunReceiveLoop_ReportsReceivedAheadOfFlush pins the fix for issue
// #101: the standby status update must report the RECEIVED (write)
// position — ahead of the durably-synced (flush) position — so PG's
// walsender shutdown (which waits on Max(write, flush)) can complete
// promptly instead of busy-looping forever. The flush position stays at
// the synced LSN so the slot's restart_lsn never advances past durable
// WAL.
func TestRunReceiveLoop_ReportsReceivedAheadOfFlush(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pp := newPipePair(t, ctx)

	sink := &recordingSink{}
	const flush = uint64(0x5000) // durable lags the received WAL
	sink.syncedLSN.Store(flush)

	statusUpdates := make(chan []byte, 32)
	go consumeServerWrites(pp.serverConn, statusUpdates)

	const walStart = uint64(0x8000)
	walData := []byte("0123456789abcdef") // 16 bytes
	receivedEnd := walStart + uint64(len(walData))

	go func() {
		emitCopyData(t, pp.sendBackend, encodeXLogData(walStart, 0x9000, 0, walData))
		time.Sleep(30 * time.Millisecond)
		// Reply-requested keepalive forces a fresh status update.
		emitCopyData(t, pp.sendBackend, encodeKeepalive(0x9000, 0, true))
		time.Sleep(60 * time.Millisecond)
		cancel()
	}()

	_ = runReceiveLoop(ctx, pp.reader, sink, time.Hour)

	write, fl, ok := lastStatusUpdate(statusUpdates, 60*time.Millisecond)
	if !ok {
		t.Fatal("no standby status update was sent")
	}
	if write != receivedEnd {
		t.Errorf("write LSN = %x, want received-end %x", write, receivedEnd)
	}
	if fl != flush {
		t.Errorf("flush LSN = %x, want synced %x", fl, flush)
	}
	if write <= fl {
		t.Errorf("write (%x) must be ahead of flush (%x) when received > synced", write, fl)
	}
}

// TestRunReceiveLoop_ServerCopyDoneIsCleanEnd: a server-initiated
// CopyDone (e.g. PG shutting down) ends the stream cleanly with
// ErrServerClosedStream, not an "unexpected message" error.
func TestRunReceiveLoop_ServerCopyDoneIsCleanEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pp := newPipePair(t, ctx)
	go drainServerWrites(pp.serverConn)

	go func() {
		time.Sleep(20 * time.Millisecond)
		pp.sendBackend.Send(&pgproto3.CopyDone{})
		_ = pp.sendBackend.Flush()
	}()

	err := runReceiveLoop(ctx, pp.reader, &recordingSink{}, time.Hour)
	if !errors.Is(err, ErrServerClosedStream) {
		t.Errorf("server CopyDone should return ErrServerClosedStream; got %v", err)
	}
}
