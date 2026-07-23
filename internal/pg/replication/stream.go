// stream.go — Stream: START_REPLICATION pump for physical WAL with feedback keepalives.
package replication

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/streaming"
)

// StreamOptions configures one Stream invocation.
type StreamOptions struct {
	// Slot is the persistent physical replication slot name. Must
	// already exist (CreatePhysicalSlot is the way).
	Slot string

	// StartLSN is the position from which to begin streaming. Pass
	// pglogrepl.LSN(0) (or its string form "0/0") to ask PG to start
	// from the slot's confirmed_flush_lsn — the resume-from-where-we-
	// were behaviour the slot is designed to provide.
	StartLSN pglogrepl.LSN

	// Timeline pins the timeline. 0 means "current" (PG decides).
	Timeline uint32

	// StatusUpdateInterval is how often we send a Standby Status
	// Update. Zero defaults to 10 s — short enough for PG to advance
	// confirmed_flush_lsn promptly, long enough to keep traffic low.
	StatusUpdateInterval time.Duration

	// InactivityTimeout overrides the streaming.Reader default for
	// the receive watchdog. Zero uses the streaming default (60 s),
	// which is appropriate for replication: PG sends a primary-
	// keepalive at least every 30 s, so a 60 s gap is suspicious.
	InactivityTimeout time.Duration

	// OnNotice forwards backend NoticeResponse messages to the
	// caller (typically piped to the dispatcher). Optional.
	OnNotice func(*pgproto3.NoticeResponse)
}

const defaultStatusInterval = 10 * time.Second

// ErrServerClosedStream is returned by Stream when the server ends the
// replication COPY itself (it sends CopyDone) — most commonly when
// PostgreSQL is shutting down. It is a CLEAN end-of-stream, not a
// failure: callers (e.g. `wal stream`) should treat it as a graceful
// stop rather than an error to retry. See issue #101.
var ErrServerClosedStream = errors.New("replication: server closed the stream (CopyDone)")

// ErrPrimaryDraining is returned when the primary is shutting down and
// its walsender busy-loops keepalives waiting for our FLUSH to confirm a
// WAL position it will never reach — the shutdown checkpoint lands in a
// partial segment, and the sink only advances SyncedLSN (flush) when a
// full 16 MiB segment commits, so our reported flush never catches up.
// PG then spins reply-requested keepalives forever, blocking its own
// fast-shutdown and, in a Patroni cluster, the demoting node's restart
// (issue #34 — verified to hang a real 3-node switchover). We end the
// stream so the walsender can exit; the reconnect routes to the new
// primary and resumes from our real flush position, gap-free (the
// unflushed partial segment is re-received from the new timeline).
var ErrPrimaryDraining = errors.New("replication: primary draining (shutdown keepalive spin); reconnecting")

var (
	// drainSpinKeepalives / drainSpinWindow define the shutdown-spin
	// signature: this many reply-requested keepalives while fully caught
	// up (nothing new to receive), within this window, with NO
	// intervening XLogData. A healthy primary sends keepalives on a
	// wal_sender_timeout/2 cadence (tens of seconds apart) and
	// interleaves XLogData, so it can never hit this; the shutdown spin
	// produces thousands per second (measured: ~1.26M in ~2.5 min).
	drainSpinKeepalives = 200
	drainSpinWindow     = 2 * time.Second
)

// XLogRecord is one chunk of WAL bytes delivered by Stream.
type XLogRecord struct {
	WALStart   pglogrepl.LSN // starting LSN of these bytes
	ServerEnd  pglogrepl.LSN // server-reported "end of WAL" at send time
	ServerTime time.Time     // server's clock at send time
	Data       []byte        // the WAL record bytes (caller should copy if retaining past the callback)
}

// WALSink is the caller's hook for processing inbound WAL records.
//
// OnRecord runs on Stream's goroutine; it must return promptly. Any
// error aborts streaming. The Sink is responsible for tracking which
// LSN it has durably stored — the LSN the agent reports back to PG
// in its standby-status updates is read from here via SyncedLSN.
//
// SyncedLSN is consulted on every standby-status interval and on
// PG-requested replies; it is the LSN at or below which the Sink
// guarantees durability. PG advances the slot's restart_lsn in
// response to this value, allowing PG to recycle older WAL.
//
// Returning a SyncedLSN of 0 means "I haven't durably stored
// anything yet"; PG won't advance the slot.
type WALSink interface {
	OnRecord(ctx context.Context, rec XLogRecord) error
	SyncedLSN() pglogrepl.LSN
}

// Stream issues START_REPLICATION SLOT <name> PHYSICAL <lsn> on a
// replication-mode connection and pumps the resulting WAL records
// through sink until ctx is cancelled, the connection drops, or
// sink returns an error.
//
// Lifecycle:
//
//  1. Send START_REPLICATION; this puts the connection into the
//     CopyBoth state. After this point the *pg.Conn is owned by
//     Stream and must not be used externally (we hijack it).
//
//  2. Receive loop: every CopyData carries either a 'w' (XLogData)
//     or a 'k' (PrimaryKeepalive). XLogData → sink.OnRecord.
//     Keepalive: if the server requested a reply, we send a status
//     update immediately; otherwise we just track the server's
//     end LSN.
//
//  3. Periodic ticker: every StatusUpdateInterval we send a
//     Standby Status Update with sink.SyncedLSN().
//
// Failure modes (all from streaming.Reader, all already documented):
//
//   - ctx cancellation: returns ctx.Err() promptly.
//   - server ErrorResponse: returns *streaming.ServerError.
//   - inactivity timeout: returns streaming.ErrInactivityTimeout.
//   - premature EOF (slot dropped, server killed): returns ErrPrematureEOF.
//   - sink error: returned wrapped.
//
// Stream takes ownership of c (via streaming.Reader's Hijack) and
// closes the underlying TCP before returning.
func Stream(ctx context.Context, c *pg.Conn, opts StreamOptions, sink WALSink) error {
	if c == nil {
		return errors.New("replication: nil connection")
	}
	if c.Mode() != pg.ModeReplication {
		return output.NewError("usage.wrong_mode",
			"Stream requires ModeReplication; got "+c.Mode().String()).Wrap(output.ErrUsage)
	}
	if opts.Slot == "" {
		return errors.New("replication: empty slot name")
	}
	if sink == nil {
		return errors.New("replication: nil sink")
	}

	// Issue START_REPLICATION on the underlying pgconn before we
	// hijack — pglogrepl handles the protocol shim. After this call
	// the connection is in CopyBoth and ready for streaming.
	startOpts := pglogrepl.StartReplicationOptions{
		Timeline: int32(opts.Timeline),
		Mode:     pglogrepl.PhysicalReplication,
	}
	if err := pglogrepl.StartReplication(ctx, c.PgConn(), opts.Slot, opts.StartLSN, startOpts); err != nil {
		return fmt.Errorf("replication: START_REPLICATION slot %q: %w", opts.Slot, err)
	}

	statusInterval := opts.StatusUpdateInterval
	if statusInterval == 0 {
		statusInterval = defaultStatusInterval
	}

	// Hijack into our resilient reader. From this point onward the
	// pg.Conn must not be touched externally.
	reader, err := streaming.New(ctx, c.PgConn(), streaming.Options{
		InactivityTimeout: opts.InactivityTimeout,
		OnNotice:          opts.OnNotice,
	})
	if err != nil {
		return fmt.Errorf("replication: hijack: %w", err)
	}
	defer reader.Close()

	return runReceiveLoop(ctx, reader, sink, statusInterval)
}

// runReceiveLoop is the inner receive loop, factored out so unit tests
// can drive it via NewWithConn against a synthetic backend.
//
// Architecture: one goroutine drives the periodic standby-status
// ticker; the main goroutine blocks on Receive. They share the
// streaming.Reader, whose Send method is mutex-protected so concurrent
// status-update writes don't race with the receive path.
func runReceiveLoop(ctx context.Context, reader *streaming.Reader, sink WALSink, statusInterval time.Duration) error {
	// written tracks the end LSN of the WAL we've RECEIVED (vs the
	// durably-flushed SyncedLSN). Shared with the ticker goroutine, so
	// it's atomic. See sendStatusUpdate / issue #101.
	var written atomic.Uint64

	// Initial status update: PG learns our position immediately after
	// START_REPLICATION. SyncedLSN of 0 is fine — it means "no
	// durability claimed yet" and doesn't advance the slot.
	if err := sendStatusUpdate(reader, sink, &written); err != nil {
		return err
	}

	// Ticker goroutine: sends periodic status updates. Lifetime is
	// bound to a child context (recvCtx) that we cancel when the receive
	// loop exits, so the goroutine always cleans up.
	//
	// Crucially, reader.Receive below also blocks on recvCtx — so when a
	// tick's status send fails, the goroutine records the error AND
	// cancels recvCtx, which UNBLOCKS the receive immediately (audit
	// #57). Previously the tick error was parked in a buffered channel
	// drained only at the top of the loop; with the watchdog disabled
	// (no InactivityTimeout) Receive could block forever, so a broken
	// send path never surfaced. tickErr is read after Receive returns to
	// distinguish "our send failed" from a plain ctx cancellation.
	recvCtx, cancelRecv := context.WithCancel(ctx)
	defer cancelRecv()
	var tickErr atomic.Pointer[error]
	go func() {
		tick := time.NewTicker(statusInterval)
		defer tick.Stop()
		for {
			select {
			case <-recvCtx.Done():
				return
			case <-tick.C:
				if err := sendStatusUpdate(reader, sink, &written); err != nil {
					tickErr.Store(&err)
					cancelRecv() // unblock the receive loop promptly
					return
				}
			}
		}
	}()

	var (
		drainKeepalives  int       // consecutive caught-up keepalives with no XLogData between
		drainWindowStart time.Time // when the current keepalive burst began
	)
	for {
		msg, err := reader.Receive(recvCtx)
		if err != nil {
			// If the receive unblocked because a tick send failed,
			// surface THAT error rather than the resulting cancellation.
			if te := tickErr.Load(); te != nil {
				return *te
			}
			return err
		}
		// Server-initiated end of COPY. PG sends CopyDone when the
		// walsender finishes — notably during a fast/smart shutdown,
		// once our reported write position has caught up (issue #101).
		// Acknowledge with our own CopyDone and end the stream cleanly
		// rather than surfacing it as an "unexpected message" error.
		if _, isDone := msg.(*pgproto3.CopyDone); isDone {
			_ = reader.Send(&pgproto3.CopyDone{})
			return ErrServerClosedStream
		}
		cd, ok := msg.(*pgproto3.CopyData)
		if !ok {
			return fmt.Errorf("replication: %w (expected CopyData; got %T)",
				streaming.ErrUnexpectedMessage, msg)
		}
		if len(cd.Data) == 0 {
			return fmt.Errorf("replication: %w (empty CopyData)", streaming.ErrUnexpectedMessage)
		}

		switch cd.Data[0] {
		case pglogrepl.XLogDataByteID:
			xld, err := pglogrepl.ParseXLogData(cd.Data[1:])
			if err != nil {
				return fmt.Errorf("replication: parse XLogData: %w", err)
			}
			rec := XLogRecord{
				WALStart:   xld.WALStart,
				ServerEnd:  xld.ServerWALEnd,
				ServerTime: xld.ServerTime,
				Data:       xld.WALData,
			}
			if err := sink.OnRecord(ctx, rec); err != nil {
				return fmt.Errorf("replication: sink.OnRecord: %w", err)
			}
			// Advance the RECEIVED position to the end of this record,
			// so our next status update tells PG we have the WAL even
			// before the sink durably flushes the (possibly partial)
			// segment. Lets a fast PG shutdown complete promptly.
			end := uint64(rec.WALStart) + uint64(len(rec.Data))
			for {
				cur := written.Load()
				if end <= cur || written.CompareAndSwap(cur, end) {
					break
				}
			}
			drainKeepalives = 0 // real WAL progress — not a shutdown spin
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			pk, err := pglogrepl.ParsePrimaryKeepaliveMessage(cd.Data[1:])
			if err != nil {
				return fmt.Errorf("replication: parse keepalive: %w", err)
			}
			// Shutdown-drain detection (issue #34): when we are fully
			// caught up (the keepalive reports no WAL beyond what we've
			// received) yet PG keeps firing keepalives in a tight burst
			// with no XLogData between them, the primary is shutting down
			// and spinning while it waits for a flush we cannot advance.
			// Bail so its walsender can exit — reconnect resumes from our
			// real flush position on the new primary.
			if uint64(pk.ServerWALEnd) <= written.Load() {
				if drainKeepalives == 0 {
					drainWindowStart = time.Now()
				}
				drainKeepalives++
				if drainKeepalives >= drainSpinKeepalives && time.Since(drainWindowStart) <= drainSpinWindow {
					return ErrPrimaryDraining
				}
			} else {
				drainKeepalives = 0
			}
			// If PG asks for a reply, send one right away so it
			// doesn't time us out.
			if pk.ReplyRequested {
				if err := sendStatusUpdate(reader, sink, &written); err != nil {
					return err
				}
			}
		default:
			// Unknown CopyData type. Not strictly a protocol violation
			// (PG could add new types) — drop silently.
		}
	}
}

// sendStatusUpdate writes a Standby Status Update reporting the
// received (write) and durably-flushed positions separately — see
// buildStatusUpdate for why the distinction matters (issue #101). The
// physical slot's restart_lsn follows the flush position, so reporting
// a write position ahead of it is safe and never opens a gap.
func sendStatusUpdate(reader *streaming.Reader, sink WALSink, written *atomic.Uint64) error {
	flush := sink.SyncedLSN()
	write := pglogrepl.LSN(written.Load())
	// The write (received) position can never sensibly be behind the
	// flushed one; clamp so a sink that reports a synced LSN ahead of
	// what we've tracked still produces a monotonic, consistent update.
	if write < flush {
		write = flush
	}
	body, err := buildStatusUpdate(write, flush)
	if err != nil {
		return err
	}
	if err := reader.Send(&pgproto3.CopyData{Data: body}); err != nil {
		return fmt.Errorf("replication: send status update: %w", err)
	}
	return nil
}

// buildStatusUpdate encodes a Standby Status Update via pglogrepl's
// helper. Encapsulated so the unit tests can compare bytes deterministically.
func buildStatusUpdate(write, flush pglogrepl.LSN) ([]byte, error) {
	// pglogrepl's SendStandbyStatusUpdate takes a pgconn directly;
	// we want to send via streaming.Reader instead, so we construct
	// the body bytes the same way pglogrepl does and ship through Send.
	// The encoding is documented and stable.
	//
	// write vs flush matters (issue #101). PG's walsender shutdown
	// (WalSndDone) ends the COPY only once Max(write, flush) reaches
	// what it sent; the physical slot's restart_lsn, however, follows
	// the FLUSH position. So we report:
	//   - write = the WAL we've RECEIVED (end of the last XLogData),
	//     which lets a fast PG shutdown release the walsender promptly
	//     instead of busy-looping forever (high CPU) waiting on us.
	//   - flush = the DURABLY-synced LSN, so PG only recycles WAL we've
	//     actually persisted — reporting write ahead of flush never
	//     advances restart_lsn, so it can't open a gap.
	const (
		standbyStatusUpdateByteID = byte('r')
	)
	const expectedLen = 1 + 8 + 8 + 8 + 8 + 1
	out := make([]byte, 0, expectedLen)
	out = append(out, standbyStatusUpdateByteID)
	out = appendUint64BE(out, uint64(write)) // write LSN
	out = appendUint64BE(out, uint64(flush)) // flush LSN
	out = appendUint64BE(out, uint64(write)) // apply LSN (caught-up)
	// PG epoch for timestamps is 2000-01-01 UTC; microseconds since.
	now := time.Now().UTC()
	pgEpoch := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	micros := uint64(now.Sub(pgEpoch).Microseconds())
	out = appendUint64BE(out, micros)
	out = append(out, 0) // 0 = no immediate reply requested
	return out, nil
}

// appendUint64BE appends n in big-endian byte order to b.
func appendUint64BE(b []byte, n uint64) []byte {
	return append(b,
		byte(n>>56), byte(n>>48), byte(n>>40), byte(n>>32),
		byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}
