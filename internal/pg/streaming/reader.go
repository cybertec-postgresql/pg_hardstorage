// Package streaming wraps a hijacked pgx connection in a message-level
// reader designed for long-running replication-protocol commands like
// BASE_BACKUP and START_REPLICATION.
//
// Design priority: reliability. Every failure mode we can think of is
// detected and surfaced through a typed error chain so callers can
// decide whether to retry, escalate, or bail.
//
// Failure modes the Reader handles explicitly:
//
//	(1) network read fails (TCP RST, peer crash)
//	   -> wrapped as net error; caller sees pg.read_failed code
//	(2) server sends ErrorResponse at any point
//	   -> surfaced as *ServerError carrying SQLSTATE + message
//	(3) caller cancels via ctx.Done()
//	   -> watcher goroutine sets a past read deadline; next Receive
//	      returns; we return ctx.Err()
//	(4) inactivity stall (server hung, TCP still alive)
//	   -> per-Receive deadline triggers; ErrInactivityTimeout returned
//	(5) reader hits EOF before the expected ReadyForQuery
//	   -> ErrPrematureEOF
//	(6) protocol mismatch (unexpected message type for the state)
//	   -> ErrUnexpectedMessage; caller decides
//	(7) async messages (NoticeResponse, ParameterStatus, NotificationResponse)
//	   -> drained transparently; optionally surfaced via OnNotice callback
//	(8) caller asks to cancel mid-stream (sink returns error, etc.)
//	   -> Reader.Cancel() sends a CancelRequest on a side-band; the
//	      next Receive returns the resulting ErrorResponse
//	(9) double-close / use-after-close
//	   -> guarded by atomic state; Receive returns ErrClosed
//	(10) process exits with the connection open
//	   -> Close sends Terminate ('X') and closes the TCP socket cleanly
//
// The Reader takes ownership of the underlying connection via
// pgconn.PgConn.Hijack() — once handed over, the original *pgconn.PgConn
// must not be used again. Callers wanting to return to normal SQL
// operation must establish a new connection.
package streaming

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// Sentinel errors. Use errors.Is to detect them.
var (
	// ErrInactivityTimeout means the server didn't send any data for at
	// least InactivityTimeout. The TCP connection might still be alive
	// (no FIN/RST received) but the server is hung or partitioned.
	ErrInactivityTimeout = errors.New("streaming: inactivity timeout (no message from server)")

	// ErrPrematureEOF means the connection closed before we saw the
	// ReadyForQuery that ends a command sequence. PG was killed
	// mid-stream, an intermediate proxy dropped us, etc.
	ErrPrematureEOF = errors.New("streaming: connection closed before end of command sequence")

	// ErrUnexpectedMessage means the server sent a backend message that
	// doesn't fit the state machine the caller is in. Wrapping this
	// over a string adds the message type to the error chain.
	ErrUnexpectedMessage = errors.New("streaming: unexpected backend message")

	// ErrClosed means Receive was called on a Reader whose Close has
	// already run, or whose ctx was cancelled and we're past clean-up.
	ErrClosed = errors.New("streaming: reader is closed")
)

// ServerError wraps a PostgreSQL ErrorResponse. SQLSTATE is the standard
// 5-character code (e.g. "57014" for query canceled, "53300" for too
// many connections). Severity is "ERROR" / "FATAL" / "PANIC" — fatal
// means the server is closing the connection, so don't expect anything
// else.
type ServerError struct {
	SQLSTATE string
	Severity string
	Message  string
	Detail   string
	Hint     string
	Position string
	Where    string
}

// Error implements the error interface.
//
// Position / Detail / Hint are appended when present.  PG only
// fills Position for parse-time errors (typically syntax_error
// 42601) and only fills Detail/Hint for the small set of
// errors where the server author thought it'd help.  Folding
// them into Error() means a single `%w`-wrapped error string
// at the top of the agent CLI tells you not just THAT a
// syntax error happened but WHERE in the command — which is
// the difference between "go read the basebackup source" and
// "the label has an unescaped quote".
func (e *ServerError) Error() string {
	var head string
	if e.SQLSTATE != "" {
		head = fmt.Sprintf("pg %s [%s]: %s", e.Severity, e.SQLSTATE, e.Message)
	} else {
		head = fmt.Sprintf("pg %s: %s", e.Severity, e.Message)
	}
	var extras []string
	if e.Position != "" && e.Position != "0" {
		extras = append(extras, "position="+e.Position)
	}
	if e.Detail != "" {
		extras = append(extras, "detail="+e.Detail)
	}
	if e.Hint != "" {
		extras = append(extras, "hint="+e.Hint)
	}
	if e.Where != "" {
		extras = append(extras, "where="+e.Where)
	}
	if len(extras) > 0 {
		head += " (" + strings.Join(extras, "; ") + ")"
	}
	return head
}

// IsFatal reports whether the server reported FATAL/PANIC severity,
// indicating the connection is unusable post-error.
func (e *ServerError) IsFatal() bool {
	return e.Severity == "FATAL" || e.Severity == "PANIC"
}

// Options configure a Reader's behaviour.
type Options struct {
	// InactivityTimeout aborts the read with ErrInactivityTimeout if no
	// message arrives in this duration.  Three regimes:
	//
	//   > 0  — use this exact value.
	//   = 0  — use DefaultInactivityTimeout.
	//   < 0  — disable the watchdog entirely; rely on PG's
	//          wal_sender_timeout (and the streamer's auto-reconnect
	//          path on real connection drop) for liveness.
	//
	// The "disabled" mode (negative value) is correct for replication
	// streams against an idle database where the operator has set
	// wal_sender_timeout = 0 — PG never sends keepalives in that
	// configuration, so the client's read deadline is the wrong place
	// to detect hangs.  For BASE_BACKUP and other bounded operations
	// where PG sends data continuously, keep the watchdog enabled.
	InactivityTimeout time.Duration

	// OnNotice receives every NoticeResponse the server emits. May be
	// nil to discard them silently. The callback runs on the Receive
	// goroutine and must return promptly.
	OnNotice func(*pgproto3.NoticeResponse)

	// OnParameterStatus receives every ParameterStatus update (e.g.
	// server-side GUC change while a command runs). Same semantics as
	// OnNotice. May be nil.
	OnParameterStatus func(*pgproto3.ParameterStatus)
}

// DefaultInactivityTimeout is the timeout applied when
// Options.InactivityTimeout is zero.  Sized for steady-state
// replication where PG's wal_sender_timeout (default 60s) makes it
// emit keepalives every ~30s: 5 minutes gives a 10× margin on
// keepalive cadence so a single missed keepalive doesn't trip the
// watchdog.  An idle database with the default
// wal_sender_timeout still keeps the stream alive indefinitely
// because PG's keepalives arrive well within this window.
//
// Operators with non-default `wal_sender_timeout = 0` (PG never
// sends keepalives) MUST disable the watchdog explicitly via the
// streamer's `--no-inactivity-timeout` flag — see issue #12 for
// the failure mode.  The streamer's preflight emits a warning
// when it detects this configuration so the conflict is visible.
const DefaultInactivityTimeout = 5 * time.Minute

// Reader is a single-goroutine consumer of a pg replication stream.
//
// Construction takes ownership of c via Hijack — the caller MUST NOT
// use c afterwards. Concurrent calls to Receive are not allowed; the
// Reader serialises against itself via the underlying TCP connection.
type Reader struct {
	netConn  net.Conn
	frontend *pgproto3.Frontend
	opts     Options

	// closed flips to 1 when Close runs. Future Receive returns ErrClosed.
	closed atomic.Int32
	// cancelWatcher stops the ctx-watcher goroutine.
	cancelWatcher context.CancelFunc
	watcherDone   chan struct{}

	// stats
	bytesReceived atomic.Uint64
	msgsReceived  atomic.Uint64

	// sendMu serialises access to the frontend's Send path so Cancel
	// from another goroutine can't interleave with our own writes.
	sendMu sync.Mutex
}

// New takes ownership of c, hijacks it, and returns a Reader bound to
// the given context. ctx controls the lifetime: cancelling ctx marks
// the reader for shutdown — any in-flight Receive returns promptly,
// and subsequent Receives return ctx.Err().
//
// The caller MUST call Close when done, even if ctx is cancelled. Close
// is safe to call multiple times and from any goroutine.
func New(ctx context.Context, c *pgconn.PgConn, opts Options) (*Reader, error) {
	if c == nil {
		return nil, errors.New("streaming: nil PgConn")
	}

	hijacked, err := c.Hijack()
	if err != nil {
		return nil, fmt.Errorf("streaming: hijack: %w", err)
	}

	return newFromConn(ctx, hijacked.Conn, hijacked.Frontend, opts), nil
}

// NewWithConn builds a Reader directly from a net.Conn, bypassing
// pgconn.PgConn.Hijack. Primary use case: test scaffolding (one half
// of a net.Pipe) and advanced callers that own the TCP connection
// without going through pgx.
//
// Caveat: this skips pgx's startup-message exchange. Pass an already-
// established connection that has completed authentication and is at
// the protocol idle state.
func NewWithConn(ctx context.Context, conn net.Conn, opts Options) *Reader {
	frontend := pgproto3.NewFrontend(conn, conn)
	return newFromConn(ctx, conn, frontend, opts)
}

// newFromConn builds a Reader from an already-existing net.Conn and a
// pre-constructed pgproto3.Frontend. Used by New (after Hijack) and by
// the test suite (after net.Pipe) so the same lifecycle/watcher logic
// is exercised by both real and synthetic transports.
func newFromConn(ctx context.Context, conn net.Conn, frontend *pgproto3.Frontend, opts Options) *Reader {
	switch {
	case opts.InactivityTimeout == 0:
		opts.InactivityTimeout = DefaultInactivityTimeout
	case opts.InactivityTimeout < 0:
		// Negative sentinel preserved as-is; Receive's deadline
		// path treats it as "no deadline".
	}
	r := &Reader{
		netConn:     conn,
		frontend:    frontend,
		opts:        opts,
		watcherDone: make(chan struct{}),
	}
	wctx, cancel := context.WithCancel(ctx)
	r.cancelWatcher = cancel
	go func() {
		defer close(r.watcherDone)
		<-wctx.Done()
		_ = r.netConn.SetReadDeadline(time.Unix(1, 0))
	}()
	return r
}

// Receive reads the next non-async backend message from the stream.
//
// Async messages (NoticeResponse, ParameterStatus, NotificationResponse,
// ParameterDescription) are handled internally and never returned —
// callers always see meaningful protocol messages.
//
// Failure modes are mapped onto typed errors:
//
//   - ErrClosed: the Reader has been closed.
//   - ctx.Err(): the caller's ctx was cancelled.
//   - ErrInactivityTimeout: no message in InactivityTimeout.
//   - ErrPrematureEOF: TCP closed before ReadyForQuery.
//   - *ServerError: PG sent ErrorResponse.
//   - other errors: low-level network / protocol failures wrapped with
//     pgproto3 / net error chains intact.
func (r *Reader) Receive(ctx context.Context) (pgproto3.BackendMessage, error) {
	if r.closed.Load() != 0 {
		return nil, ErrClosed
	}
	for {
		// Caller may have cancelled ctx between Receives.
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Set a per-receive read deadline so a hung server is detected
		// even when the caller's ctx has no deadline. This deadline is
		// independent of the watcher goroutine's "ctx-cancelled"
		// deadline (Unix(1, 0)); whichever fires first wins.
		//
		// Negative InactivityTimeout disables the deadline entirely —
		// the watchdog stays on the watcher-goroutine path only.
		// Required for replication streams against PG instances with
		// wal_sender_timeout = 0 (PG never sends keepalives, so a
		// read deadline becomes a false-positive on idle databases —
		// see issue #12).
		if r.opts.InactivityTimeout > 0 {
			deadline := time.Now().Add(r.opts.InactivityTimeout)
			if err := r.netConn.SetReadDeadline(deadline); err != nil {
				return nil, fmt.Errorf("streaming: SetReadDeadline: %w", err)
			}
		} else {
			// Clear any deadline a previous Receive set, in case the
			// caller flips InactivityTimeout to negative mid-stream.
			if err := r.netConn.SetReadDeadline(time.Time{}); err != nil {
				return nil, fmt.Errorf("streaming: clear ReadDeadline: %w", err)
			}
		}

		msg, err := r.frontend.Receive()
		if err != nil {
			return nil, r.classifyReadError(ctx, err)
		}
		r.msgsReceived.Add(1)

		switch m := msg.(type) {
		case *pgproto3.ErrorResponse:
			return nil, serverErrorFrom(m)
		case *pgproto3.NoticeResponse:
			if r.opts.OnNotice != nil {
				r.opts.OnNotice(m)
			}
			continue
		case *pgproto3.ParameterStatus:
			if r.opts.OnParameterStatus != nil {
				r.opts.OnParameterStatus(m)
			}
			continue
		case *pgproto3.NotificationResponse:
			// LISTEN/NOTIFY notifications can arrive at any moment.
			// Replication streams don't normally use them, but be
			// defensive: silently drain rather than confuse the caller.
			continue
		case *pgproto3.CopyData:
			// Track bytes for stats. CopyData is the bulk-data carrier
			// for BASE_BACKUP tar streams.
			r.bytesReceived.Add(uint64(len(m.Data)))
			return m, nil
		default:
			return msg, nil
		}
	}
}

// classifyReadError maps a pgproto3.Receive error onto our typed errors.
// It needs to consider:
//   - was ctx cancelled by the caller?
//   - was the deadline our inactivity watchdog or the watcher's "force
//     return now" deadline?
//   - did we EOF the stream?
//   - is it a real network error?
func (r *Reader) classifyReadError(ctx context.Context, err error) error {
	// ctx cancelled is the most user-facing case: surface ctx.Err.
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrPrematureEOF
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		// Timeout could be: (a) our inactivity watchdog tripped, or
		// (b) the watcher goroutine set a past deadline because ctx
		// was cancelled. Distinguish by re-checking ctx.
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		return fmt.Errorf("%w (after %s)", ErrInactivityTimeout, r.opts.InactivityTimeout)
	}
	return fmt.Errorf("streaming: receive: %w", err)
}

// Send writes a frontend message to the server. Used by replication-
// command code (e.g. CopyData for two-way streams). The send path is
// mutexed because Cancel can be invoked from another goroutine.
func (r *Reader) Send(msg pgproto3.FrontendMessage) error {
	if r.closed.Load() != 0 {
		return ErrClosed
	}
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	r.frontend.Send(msg)
	return r.frontend.Flush()
}

// Stats returns observed counters. Useful for progress events.
func (r *Reader) Stats() Stats {
	return Stats{
		BytesReceived: r.bytesReceived.Load(),
		MsgsReceived:  r.msgsReceived.Load(),
	}
}

// Stats is a Reader's running counters.
type Stats struct {
	BytesReceived uint64
	MsgsReceived  uint64
}

// Close shuts down the reader. It:
//   - flips the closed flag (subsequent Receive returns ErrClosed);
//   - sends a Terminate('X') message best-effort to let the server
//     know we're going away cleanly (with a hard write deadline so a
//     stuck pipe / dead peer can't hang Close);
//   - cancels the watcher goroutine and waits for it to exit;
//   - closes the underlying TCP connection.
//
// Close is idempotent and safe to call from any goroutine.
func (r *Reader) Close() error {
	if !r.closed.CompareAndSwap(0, 1) {
		return nil // already closed
	}

	// Best-effort polite Terminate. We set a tight write deadline so a
	// dead peer (or a synchronous pipe in tests) can't make Close hang
	// indefinitely. Errors are intentionally ignored — the server gets
	// a TCP FIN whether we Terminate or not, and the dead-peer case is
	// exactly when we don't care about niceness.
	_ = r.netConn.SetWriteDeadline(time.Now().Add(closeTerminateBudget))
	r.sendMu.Lock()
	r.frontend.Send(&pgproto3.Terminate{})
	_ = r.frontend.Flush()
	r.sendMu.Unlock()

	r.cancelWatcher()
	<-r.watcherDone

	// Forcing a deadline in the past wakes any in-flight Receive on
	// another goroutine before we close the underlying conn.
	_ = r.netConn.SetDeadline(time.Unix(1, 0))
	return r.netConn.Close()
}

// closeTerminateBudget caps how long Close waits when emitting the
// polite Terminate message. Real TCP either accepts the bytes or fails
// immediately; net.Pipe in tests blocks if no one is reading. Either
// way we don't want Close to hang.
const closeTerminateBudget = 100 * time.Millisecond

// serverErrorFrom translates a pgproto3.ErrorResponse into our typed
// *ServerError. We keep only the fields we expect to use; PG sends
// many more (line numbers, internal detail) but those add noise without
// adding diagnostic value for our tooling.
func serverErrorFrom(m *pgproto3.ErrorResponse) *ServerError {
	return &ServerError{
		SQLSTATE: m.Code,
		Severity: m.Severity,
		Message:  m.Message,
		Detail:   m.Detail,
		Hint:     m.Hint,
		Position: fmt.Sprintf("%d", m.Position),
		Where:    m.Where,
	}
}
