// stream.go — Sink interface + START_REPLICATION loop for logical-decoding XLogData frames.
package logicalreceiver

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

// pgproto3CopyData aliases the real type so messageAsCopyData stays
// import-tidy.
type pgproto3CopyData = pgproto3.CopyData

// Sink consumes XLogData frames produced by the logical receiver. v0.1
// ships exactly one Sink implementation (chunked, in
// internal/logical/sinks/chunked); adds Kafka, Pub/Sub, webhook,
// PII-redactor.
//
// OnRecord runs on the receive goroutine. It MUST return promptly —
// any blocking work belongs to a goroutine the sink owns. Sinks that
// batch (the chunked sink does, for CAS efficiency) drain their batch
// async and update SyncedLSN once durable.
type Sink interface {
	// OnRecord receives one logical XLogData frame. The bytes are the
	// pgoutput protocol payload (or whatever plugin the slot is
	// configured with). The Sink is free to decode, store, transform
	// — its choice.
	OnRecord(ctx context.Context, rec Record) error

	// SyncedLSN reports the LSN through which the Sink has durably
	// committed. The receive loop forwards this to PG via Standby
	// Status Updates so the slot's confirmed_flush_lsn advances and
	// retained WAL can be released.
	SyncedLSN() pglogrepl.LSN

	// Flush durably commits any buffered-but-uncommitted batch and
	// advances SyncedLSN to cover it. The receive loop calls Flush on
	// the status-update cadence and once more at shutdown — without a
	// cadence-driven flush a publication quieter than the sink's batch
	// threshold would buffer forever, SyncedLSN would never move, and
	// PG would retain WAL indefinitely. Flush on an empty batch is a
	// no-op. (Every sink in internal/logical/sinks already implements
	// this; it is part of the contract their comments already claim.)
	Flush(ctx context.Context) error
}

// Record is one logical XLogData frame from the wire. WALStart is the
// PG LSN at which the record's bytes start; ServerWALEnd / ServerTime
// are PG's bookkeeping fields useful for lag reporting.
type Record struct {
	WALStart     pglogrepl.LSN
	ServerWALEnd pglogrepl.LSN
	ServerTime   time.Time
	Data         []byte
}

// StreamOptions configures Stream. PluginArgs feed pgoutput's
// publication selection; v0.1 expects the caller to pass
// `proto_version` and `publication_names` explicitly so a future
// pgoutput revision can be selected without changing this surface.
type StreamOptions struct {
	Slot                 string
	StartLSN             pglogrepl.LSN
	PluginArgs           []string
	StatusUpdateInterval time.Duration
	InactivityTimeout    time.Duration
}

// Stream runs the logical-receive loop. The shape mirrors
// internal/pg/replication.Stream: ctx cancels cleanly, the sink owns
// durability, status updates fire on a ticker. Differences from the
// physical receiver:
//
//   - Plugin args (publication list) ride in the START_REPLICATION
//     SLOT command rather than being implicit.
//   - The receive loop hands each XLogData payload directly to the
//     sink — there's no equivalent of the 16 MiB segment alignment
//     that the physical path enforces.
//
// Stream takes ownership of conn from this point forward; the caller
// must NOT touch the underlying *pg.Conn or *pgconn.PgConn after
// Stream is invoked.
func Stream(ctx context.Context, conn *pg.Conn, opts StreamOptions, sink Sink) (retErr error) {
	if conn == nil {
		return errors.New("logicalreceiver: nil connection")
	}
	// Stream owns conn's lifecycle (see the doc comment: the caller
	// must not touch it after Stream is invoked). Close it on every
	// return path so the replication connection — and therefore the
	// logical slot it holds active — is released. Without this the
	// runner's backoff-retry supervisor leaks one connection per
	// attempt and every retry after the first fails with
	// `replication slot is active` (SQLSTATE 55006), permanently
	// wedging logical replication after any transient error.
	//
	// A detached, time-bounded context: by the time Stream returns
	// the caller's ctx is usually already cancelled, which would
	// otherwise make Close a no-op send on a dead context.
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = conn.Close(closeCtx)
		closeCancel()
	}()
	if sink == nil {
		return errors.New("logicalreceiver: nil sink")
	}
	if opts.Slot == "" {
		return errors.New("logicalreceiver: empty slot name")
	}
	if opts.StatusUpdateInterval <= 0 {
		opts.StatusUpdateInterval = 10 * time.Second
	}

	pgc := conn.PgConn()
	if err := pglogrepl.StartReplication(ctx, pgc, opts.Slot, opts.StartLSN, pglogrepl.StartReplicationOptions{
		Mode:       pglogrepl.LogicalReplication,
		PluginArgs: opts.PluginArgs,
	}); err != nil {
		return fmt.Errorf("logicalreceiver: START_REPLICATION %s: %w", opts.Slot, err)
	}

	// Final flush + standby status update on every exit path. The
	// receive loop only flushes/reports between received messages —
	// once the backlog drains it blocks in ReceiveMessage and exits
	// via inactivity-timeout or ctx-cancel WITHOUT ever looping back.
	// Without this defer the sink's last buffered batch is never
	// committed by Stream, the slot's confirmed_flush_lsn lags the
	// data consumed (so the next session re-delivers the tail), and a
	// stream shorter than one StatusUpdateInterval never advances the
	// slot at all — PG retains that WAL indefinitely. Detached
	// context: the caller's ctx is usually already cancelled here.
	// Runs before the conn.Close defer above (defers are LIFO).
	defer func() {
		// Surface a failed final flush instead of dropping it (it was
		// `_ = sink.Flush(...)` before — poor-error-handling audit #3):
		// the last buffered batch wasn't durably committed, and Stream
		// would otherwise return success. Join onto retErr so a clean
		// shutdown's context.Canceled — which the supervisor detects via
		// errors.Is — still shows through. On a flush failure, skip the
		// status update: we must not report an LSN we didn't durably
		// reach.
		if ferr := finalCommit(sink); ferr != nil {
			retErr = errors.Join(retErr, ferr)
			return
		}
		sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer scancel()
		lsn := sink.SyncedLSN()
		if serr := pglogrepl.SendStandbyStatusUpdate(sctx, pgc, pglogrepl.StandbyStatusUpdate{
			WALWritePosition: lsn,
			WALFlushPosition: lsn,
			WALApplyPosition: lsn,
		}); serr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("logicalreceiver: final standby status update: %w", serr))
		}
	}()

	// flushAndReport durably commits the sink's batch then reports the
	// resulting SyncedLSN to PG. Called on the status-update cadence
	// (and on reply-requested keepalives) so confirmed_flush_lsn
	// tracks consumed data even for a low-volume publication.
	flushAndReport := func(c context.Context) error {
		if err := sink.Flush(c); err != nil {
			return fmt.Errorf("logicalreceiver: flush: %w", err)
		}
		lsn := sink.SyncedLSN()
		if err := pglogrepl.SendStandbyStatusUpdate(c, pgc, pglogrepl.StandbyStatusUpdate{
			WALWritePosition: lsn,
			WALFlushPosition: lsn,
			WALApplyPosition: lsn,
		}); err != nil {
			return fmt.Errorf("logicalreceiver: status update: %w", err)
		}
		return nil
	}

	// Periodic status deadline: the loop reports at least this often
	// so a quiet publication (or wal_sender_timeout=0, where PG never
	// sends a reply-requested keepalive) still advances the slot's
	// confirmed_flush_lsn. We drive it by bounding each ReceiveMessage
	// with a per-iteration deadline = now + StatusUpdateInterval. When
	// ReceiveMessage hits THAT deadline we flush/report and loop — we do
	// NOT treat it as an error. See nextStatus tracking below.
	//
	// Sends stay on this (the sole) goroutine: pgconn's frontend is not
	// safe for a concurrent Send while ReceiveMessage blocks, so we must
	// not spin up a writer goroutine (the previous statusReq goroutine
	// only signalled — and was never observed while ReceiveMessage was
	// blocked on a quiet stream, so status never fired: audit #56 /
	// #27's leaked goroutine).
	nextStatus := time.Now().Add(opts.StatusUpdateInterval)

	// Inactivity deadline: bound the total time we'll wait without ANY
	// server traffic before treating it as a stuck connection. Distinct
	// from the status cadence: a status-deadline hit is normal (flush &
	// continue); an inactivity-deadline hit is fatal.
	inactivityDeadline := func() time.Time {
		if opts.InactivityTimeout <= 0 {
			return time.Time{} // no deadline; rely on ctx cancellation
		}
		return time.Now().Add(opts.InactivityTimeout)
	}
	lastTraffic := time.Now()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Fire the periodic flush/status whenever the cadence elapses,
		// independent of whether a message just arrived (audit #56).
		if !time.Now().Before(nextStatus) {
			if err := flushAndReport(ctx); err != nil {
				return err
			}
			nextStatus = time.Now().Add(opts.StatusUpdateInterval)
		}

		// Bound this ReceiveMessage by the SOONER of the next status
		// tick and the inactivity deadline, so a quiet stream wakes to
		// flush/report on the status cadence rather than blocking until
		// server traffic. Cancel as soon as ReceiveMessage returns —
		// `defer cancel()` here would leak one context+timer per message.
		waitUntil := nextStatus
		inact := inactivityDeadline()
		if !inact.IsZero() && inact.Before(waitUntil) {
			waitUntil = inact
		}
		recvCtx, cancel := context.WithDeadline(ctx, waitUntil)
		msg, err := pgc.ReceiveMessage(recvCtx)
		cancel()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				// Which deadline fired? If the inactivity budget is
				// exhausted (no traffic for the whole window), that's
				// fatal. Otherwise it's just the status cadence: flush,
				// report, and loop back to keep receiving.
				if !inact.IsZero() && !time.Now().Before(inact) &&
					time.Since(lastTraffic) >= opts.InactivityTimeout {
					return fmt.Errorf("logicalreceiver: inactivity timeout (%s)", opts.InactivityTimeout)
				}
				if err := flushAndReport(ctx); err != nil {
					return err
				}
				nextStatus = time.Now().Add(opts.StatusUpdateInterval)
				continue
			}
			return fmt.Errorf("logicalreceiver: receive: %w", err)
		}
		lastTraffic = time.Now()

		// Handle the non-CopyData protocol messages the walsender can
		// send mid-stream. Previously EVERYTHING that wasn't CopyData
		// was silently ignored (`continue`), so an ERROR-severity
		// ErrorResponse or a server CopyDone was swallowed and the loop
		// re-blocked in ReceiveMessage forever (audit #26). Mirror the
		// physical receiver's taxonomy via controlMessageAction (a pure
		// function so the classification is unit-testable without a live
		// walsender).
		switch act, cerr := controlMessageAction(msg); act {
		case ctrlError:
			return cerr
		case ctrlDone:
			// Server-initiated clean end of COPY (e.g. PG shutting down).
			// Not a failure — return nil and let the exit defers do the
			// final flush + status update.
			return nil
		case ctrlIgnore:
			// Non-fatal control message (e.g. NoticeResponse); keep
			// streaming.
			continue
		}

		// Look at the message body. pgconn surfaces CopyData for
		// streaming-protocol frames; everything else is unexpected
		// during a logical-replication conversation.
		cd, ok := messageAsCopyData(msg)
		if !ok {
			// Some protocol message we don't care about; ignore.
			continue
		}
		if len(cd) == 0 {
			continue
		}
		switch cd[0] {
		case pglogrepl.XLogDataByteID:
			xld, err := pglogrepl.ParseXLogData(cd[1:])
			if err != nil {
				return fmt.Errorf("logicalreceiver: parse XLogData: %w", err)
			}
			rec := Record{
				WALStart:     xld.WALStart,
				ServerWALEnd: xld.ServerWALEnd,
				ServerTime:   xld.ServerTime,
				Data:         xld.WALData,
			}
			if err := sink.OnRecord(ctx, rec); err != nil {
				return fmt.Errorf("logicalreceiver: sink: %w", err)
			}
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			pk, err := pglogrepl.ParsePrimaryKeepaliveMessage(cd[1:])
			if err != nil {
				return fmt.Errorf("logicalreceiver: parse keepalive: %w", err)
			}
			if pk.ReplyRequested {
				if err := flushAndReport(ctx); err != nil {
					return err
				}
			}
		default:
			// Unknown protocol message — log and continue. The PG
			// protocol can grow new message types; the safe default
			// is "ignore unknown" so a server-side rev doesn't break
			// the consumer immediately.
		}
	}
}

// finalCommit durably flushes the sink's last buffered batch on Stream's
// exit path, returning a wrapped error on failure. Extracted from the
// exit defer so the "don't swallow the final flush" guarantee
// (poor-error-handling audit #3) is unit-testable without a live
// replication connection. Uses a detached, time-bounded context because
// the caller's ctx is usually already cancelled by the time Stream
// returns.
func finalCommit(sink Sink) error {
	fctx, fcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer fcancel()
	if err := sink.Flush(fctx); err != nil {
		return fmt.Errorf("logicalreceiver: final flush on shutdown: %w", err)
	}
	return nil
}

// controlAction is what the receive loop should do with a non-CopyData
// backend message.
type controlAction int

const (
	// ctrlNone: not a control message we handle here — fall through to
	// the CopyData path.
	ctrlNone controlAction = iota
	// ctrlError: fatal server ErrorResponse — return the accompanying err.
	ctrlError
	// ctrlDone: server CopyDone — clean end of stream, return nil.
	ctrlDone
	// ctrlIgnore: non-fatal control message (NoticeResponse) — continue.
	ctrlIgnore
)

// controlMessageAction classifies a backend message the logical
// receive loop got from ReceiveMessage. It mirrors the physical
// receiver's taxonomy so a mid-stream ErrorResponse or CopyDone isn't
// swallowed (audit #26): an ErrorResponse aborts with a descriptive
// error, a CopyDone ends the stream cleanly, a NoticeResponse is
// ignored, and anything else falls through (ctrlNone) to the CopyData
// handling. Pure function — no I/O — so it's unit-testable without a
// live walsender.
func controlMessageAction(msg pgproto3.BackendMessage) (controlAction, error) {
	switch m := msg.(type) {
	case *pgproto3.ErrorResponse:
		return ctrlError, fmt.Errorf("logicalreceiver: server error: %s (SQLSTATE %s): %s",
			m.Severity, m.Code, m.Message)
	case *pgproto3.CopyDone:
		return ctrlDone, nil
	case *pgproto3.NoticeResponse:
		return ctrlIgnore, nil
	default:
		return ctrlNone, nil
	}
}

// messageAsCopyData unwraps a pgproto3.CopyData into its payload
// bytes. Returns ok=false for anything else (the v0.1 logical loop
// only cares about CopyData; ErrorResponse / NoticeResponse handling
// would slot in here alongside the physical receiver's
// taxonomy).
//
// Mirrors the physical receiver's `msg.(*pgproto3.CopyData)` cast.
// We accept the import-cycle concern for clarity: pgproto3 is a
// stable, low-churn dep already pulled by the rest of the agent.
func messageAsCopyData(msg interface{}) ([]byte, bool) {
	cd, ok := msg.(*pgproto3CopyData)
	if !ok {
		return nil, false
	}
	return cd.Data, true
}

// pgproto3CopyData is a thin alias to keep the unwrap helper agnostic
// of the import path. We re-export the type here via a typed nil
// reference at init so the compiler binds it to the real
// pgproto3.CopyData definition.
//
// In practice: import the real type and use it directly.

// uint64FromBytes is a small helper used by tests that synthesise
// XLogData frames; kept here so the test file can build a frame
// without re-implementing the binary layout.
func uint64FromBytes(b []byte) uint64 {
	if len(b) < 8 {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}
