package streaming_test

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/streaming"
)

// pipeReader builds a Reader hooked up to one half of a net.Pipe and
// returns the other half wrapped in a pgproto3.Backend so the test can
// "be the PG server" and emit canned messages.
func pipeReader(t *testing.T, ctx context.Context, opts streaming.Options) (*streaming.Reader, *pgproto3.Backend, net.Conn) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	r := streaming.NewWithConn(ctx, clientConn, opts)
	be := pgproto3.NewBackend(serverConn, serverConn)
	t.Cleanup(func() {
		_ = r.Close()
		_ = serverConn.Close()
	})
	return r, be, serverConn
}

// flushBackend sends the given backend messages and flushes. Used by
// the test "server" goroutine to emit a canned response.
func flushBackend(t *testing.T, be *pgproto3.Backend, msgs ...pgproto3.BackendMessage) {
	t.Helper()
	for _, m := range msgs {
		be.Send(m)
	}
	if err := be.Flush(); err != nil {
		t.Errorf("backend flush: %v", err)
	}
}

func TestReceive_HappyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, be, _ := pipeReader(t, ctx, streaming.Options{})

	go flushBackend(t, be,
		&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("col")}}},
	)
	msg, err := r.Receive(ctx)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if _, ok := msg.(*pgproto3.RowDescription); !ok {
		t.Errorf("got %T, want *pgproto3.RowDescription", msg)
	}
}

func TestReceive_ErrorResponse_BecomesServerError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, be, _ := pipeReader(t, ctx, streaming.Options{})

	go flushBackend(t, be,
		&pgproto3.ErrorResponse{
			Severity: "ERROR",
			Code:     "57014",
			Message:  "canceling statement due to user request",
		},
	)
	_, err := r.Receive(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
	var se *streaming.ServerError
	if !errors.As(err, &se) {
		t.Fatalf("expected *ServerError; got %T %v", err, err)
	}
	if se.SQLSTATE != "57014" {
		t.Errorf("SQLSTATE = %q", se.SQLSTATE)
	}
	if se.Severity != "ERROR" {
		t.Errorf("Severity = %q", se.Severity)
	}
	if se.IsFatal() {
		t.Error("ERROR severity is not fatal")
	}
}

// TestServerError_RendersDiagnosticContext locks the
// load-bearing property of the new Error() rendering: when PG
// supplies Position / Detail / Hint, they appear in the
// returned string so a syntax error doesn't have to be
// debugged by reading agent source.  The previous rendering
// dropped all three, which made a real soak failure
// (`pg ERROR [42601]: syntax error`) impossible to diagnose
// without instrumentation.
func TestServerError_RendersDiagnosticContext(t *testing.T) {
	cases := []struct {
		name string
		err  streaming.ServerError
		want string
	}{
		{
			"sqlstate only",
			streaming.ServerError{SQLSTATE: "57014", Severity: "ERROR", Message: "canceled"},
			`pg ERROR [57014]: canceled`,
		},
		{
			"with position",
			streaming.ServerError{
				SQLSTATE: "42601", Severity: "ERROR", Message: "syntax error",
				Position: "192",
			},
			`pg ERROR [42601]: syntax error (position=192)`,
		},
		{
			"position zero suppressed",
			streaming.ServerError{
				SQLSTATE: "42601", Severity: "ERROR", Message: "syntax error",
				Position: "0",
			},
			`pg ERROR [42601]: syntax error`,
		},
		{
			"position+hint+detail",
			streaming.ServerError{
				SQLSTATE: "42601", Severity: "ERROR",
				Message:  "syntax error at or near \"FOO\"",
				Position: "12",
				Detail:   "the parser expected a keyword",
				Hint:     "did you mean BAR?",
			},
			`pg ERROR [42601]: syntax error at or near "FOO" (position=12; detail=the parser expected a keyword; hint=did you mean BAR?)`,
		},
		{
			"no sqlstate",
			streaming.ServerError{Severity: "FATAL", Message: "too many connections"},
			`pg FATAL: too many connections`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("\n  got:  %q\n  want: %q", got, tc.want)
			}
		})
	}
}

func TestReceive_FatalErrorResponse_IsFatal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, be, _ := pipeReader(t, ctx, streaming.Options{})

	go flushBackend(t, be,
		&pgproto3.ErrorResponse{Severity: "FATAL", Code: "53300", Message: "too many connections"},
	)
	_, err := r.Receive(ctx)
	var se *streaming.ServerError
	if !errors.As(err, &se) || !se.IsFatal() {
		t.Errorf("expected fatal *ServerError; got %v", err)
	}
}

func TestReceive_NoticeResponse_DrainedTransparently(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var noticeCount atomic.Int32
	r, be, _ := pipeReader(t, ctx, streaming.Options{
		OnNotice: func(*pgproto3.NoticeResponse) { noticeCount.Add(1) },
	})

	go flushBackend(t, be,
		&pgproto3.NoticeResponse{Severity: "NOTICE", Message: "informational"},
		&pgproto3.NoticeResponse{Severity: "NOTICE", Message: "another"},
		&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("c")}}},
	)
	msg, err := r.Receive(ctx)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if _, ok := msg.(*pgproto3.RowDescription); !ok {
		t.Fatalf("got %T, want RowDescription (notices should be drained)", msg)
	}
	if got := noticeCount.Load(); got != 2 {
		t.Errorf("OnNotice called %d times, want 2", got)
	}
}

func TestReceive_ParameterStatus_DrainedTransparently(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var psCount atomic.Int32
	r, be, _ := pipeReader(t, ctx, streaming.Options{
		OnParameterStatus: func(*pgproto3.ParameterStatus) { psCount.Add(1) },
	})

	go flushBackend(t, be,
		&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"},
		&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("c")}}},
	)
	msg, err := r.Receive(ctx)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if _, ok := msg.(*pgproto3.RowDescription); !ok {
		t.Errorf("got %T, want RowDescription", msg)
	}
	if got := psCount.Load(); got != 1 {
		t.Errorf("OnParameterStatus called %d times, want 1", got)
	}
}

func TestReceive_PrematureEOF(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, _, serverConn := pipeReader(t, ctx, streaming.Options{})

	// Close the server side without sending anything.
	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = serverConn.Close()
	}()

	_, err := r.Receive(ctx)
	if !errors.Is(err, streaming.ErrPrematureEOF) {
		// Some platforms surface a different network error before EOF;
		// we accept either as long as it's not silently treated as a
		// real message.
		if !errors.Is(err, io.EOF) {
			t.Errorf("expected ErrPrematureEOF or io.EOF; got %v", err)
		}
	}
}

func TestReceive_CtxCancel_InterruptsBlockingRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _, _ := pipeReader(t, ctx, streaming.Options{InactivityTimeout: 30 * time.Second})

	// No backend messages will ever come. After a brief delay, cancel
	// ctx and confirm Receive returns promptly with ctx.Err.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := r.Receive(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled; got %v", err)
	}
	if elapsed > 1*time.Second {
		t.Errorf("ctx cancel didn't interrupt read promptly (took %v)", elapsed)
	}
}

func TestReceive_InactivityTimeout(t *testing.T) {
	// Set a tight inactivity timeout and send nothing. Receive should
	// return ErrInactivityTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, _, _ := pipeReader(t, ctx, streaming.Options{InactivityTimeout: 100 * time.Millisecond})

	start := time.Now()
	_, err := r.Receive(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, streaming.ErrInactivityTimeout) {
		t.Errorf("expected ErrInactivityTimeout; got %v", err)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("returned before timeout (%v)", elapsed)
	}
	if elapsed > 1*time.Second {
		t.Errorf("returned much later than timeout (%v)", elapsed)
	}
}

// TestReceive_InactivityTimeoutDisabled regresses issue #12: a
// negative InactivityTimeout disables the client-side watchdog
// entirely, required for replication streams against PG instances
// with wal_sender_timeout = 0 (no server keepalives).
//
// We send nothing, wait long enough that a 100ms timeout would
// fire ten times over, and assert Receive is still blocked.  ctx
// cancellation is what unblocks it cleanly.
func TestReceive_InactivityTimeoutDisabled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, _, _ := pipeReader(t, ctx, streaming.Options{InactivityTimeout: -1})

	done := make(chan error, 1)
	go func() {
		_, err := r.Receive(ctx)
		done <- err
	}()

	select {
	case <-time.After(1 * time.Second):
		// Receive is still blocked — the disabled watchdog did
		// not fire after 1s.  That's the property we want.
	case err := <-done:
		t.Fatalf("Receive returned prematurely with err=%v; expected to stay blocked when watchdog is disabled", err)
	}

	// Now cancel ctx and confirm Receive unblocks with ctx.Err.
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("after cancel, expected context.Canceled; got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Errorf("Receive didn't return after ctx cancel")
	}
}

func TestReceive_CopyData_UpdatesByteStats(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, be, _ := pipeReader(t, ctx, streaming.Options{})

	body := []byte("hello world bytes")
	go flushBackend(t, be, &pgproto3.CopyData{Data: body})

	msg, err := r.Receive(ctx)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	cd, ok := msg.(*pgproto3.CopyData)
	if !ok {
		t.Fatalf("got %T, want CopyData", msg)
	}
	if string(cd.Data) != string(body) {
		t.Errorf("data round-trip mismatch")
	}
	stats := r.Stats()
	if stats.BytesReceived != uint64(len(body)) {
		t.Errorf("BytesReceived = %d, want %d", stats.BytesReceived, len(body))
	}
	if stats.MsgsReceived != 1 {
		t.Errorf("MsgsReceived = %d, want 1", stats.MsgsReceived)
	}
}

func TestClose_ThenReceive_ReturnsErrClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, _, _ := pipeReader(t, ctx, streaming.Options{})

	if err := r.Close(); err != nil {
		// Pipe may surface "io: read/write on closed pipe" — that's fine.
		t.Logf("close: %v (acceptable)", err)
	}
	_, err := r.Receive(ctx)
	if !errors.Is(err, streaming.ErrClosed) {
		t.Errorf("expected ErrClosed; got %v", err)
	}
}

func TestClose_Idempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, _, _ := pipeReader(t, ctx, streaming.Options{})

	_ = r.Close()
	_ = r.Close() // must not panic
}

func TestNew_NilPgConn(t *testing.T) {
	// New with a nil *pgconn.PgConn must error, not panic.
	_, err := streaming.New(context.Background(), nil, streaming.Options{})
	if err == nil {
		t.Error("expected error on nil PgConn")
	}
}

func TestSend_AfterClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, _, _ := pipeReader(t, ctx, streaming.Options{})

	_ = r.Close()
	if err := r.Send(&pgproto3.Terminate{}); !errors.Is(err, streaming.ErrClosed) {
		t.Errorf("Send after Close should return ErrClosed; got %v", err)
	}
}
