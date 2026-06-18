// Package replication wraps PostgreSQL's streaming-replication
// protocol for two purposes:
//
//   - Slot management (Create / Drop / Get)
//   - Continuous WAL receive (Stream) over a persistent physical slot
//
// We layer over jackc/pglogrepl for protocol-message parsing and over
// our own streaming.Reader for the receive-loop resilience contract
// (ctx cancellation, server ErrorResponse, premature EOF, inactivity
// timeout, async-message draining — see internal/pg/streaming for
// the full taxonomy).
//
// Slot semantics:
//
//   - Slots persist across connections. PG holds WAL until the slot's
//     restart_lsn advances (which happens when a standby ack'd that
//     LSN as flushed). A persistent slot is exactly what we want
//     for resumable continuous archiving.
//
//   - Reading pg_replication_slots requires a regular-mode connection
//     (the view is plain SQL). Creating / dropping slots happens via
//     the replication-protocol command vocabulary on a replication-mode
//     connection. The two operations therefore need separate *pg.Conn
//     instances; that's the caller's responsibility.
package replication

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

// SlotInfo describes one replication slot. Field set covers the
// columns we need today; we can grow as features land (failover slot,
// two-phase, ...).
type SlotInfo struct {
	Name              string
	Type              string // "physical" or "logical"
	Active            bool
	RestartLSN        string // empty when slot has never been used
	ConfirmedFlushLSN string // populated for logical slots only
	Plugin            string // populated for logical slots only
}

// CreatePhysicalSlot creates a persistent physical replication slot
// named name on a replication-mode connection. Idempotent: if the slot
// already exists with the same shape we don't error; the caller can
// verify via Get if they care about pre-existing state.
//
// "Persistent" is the operational requirement: PG holds WAL on the
// agent's behalf until the slot's restart_lsn advances. A temporary
// slot would drop on disconnect, defeating the purpose.
//
// Without RESERVE_WAL, the slot's restart_lsn is initially NULL —
// PG only starts reserving WAL on the first START_REPLICATION.
// Use CreatePhysicalSlotReserveWAL when you need restart_lsn to
// be populated immediately after creation (e.g., the
// slot-continuity gap calculation in the leader-follow loop).
func CreatePhysicalSlot(ctx context.Context, c *pg.Conn, name string) error {
	if c == nil {
		return errors.New("replication: nil connection")
	}
	if c.Mode() != pg.ModeReplication {
		return output.NewError("usage.wrong_mode",
			"CreatePhysicalSlot requires ModeReplication; got "+c.Mode().String()).Wrap(output.ErrUsage)
	}
	if !pg.ValidIdentifier(name) {
		return fmt.Errorf("replication: slot name %q is not a valid PG identifier "+
			"(letter/underscore start, then [a-z0-9_], ≤63 chars)", name)
	}

	// pglogrepl's CreateReplicationSlot supports physical via the
	// SlotType option; we pass through with no plugin.
	_, err := pglogrepl.CreateReplicationSlot(ctx, c.PgConn(), name, "" /* plugin */, pglogrepl.CreateReplicationSlotOptions{
		Mode: pglogrepl.PhysicalReplication,
	})
	if err != nil {
		// 42710 (duplicate_object) means the slot is already there.
		// Treat that as a non-error so Create is idempotent.
		if isDuplicateObjectErr(err) {
			return nil
		}
		return fmt.Errorf("replication: create slot %q: %w", name, err)
	}
	return nil
}

// CreatePhysicalSlotReserveWAL creates a persistent physical
// replication slot with the RESERVE_WAL flag set. The flag tells
// PG to start reserving WAL at the current xlog position
// immediately, so the slot's restart_lsn is populated as soon as
// the call returns (rather than at the first START_REPLICATION).
//
// pglogrepl.CreateReplicationSlot doesn't expose RESERVE_WAL, so
// we issue the raw command via Exec. The wire form is:
//
//	CREATE_REPLICATION_SLOT <name> PHYSICAL RESERVE_WAL
//
// Idempotent on already-exists (SQLSTATE 42710 → returns nil).
//
// Used by the slot-continuity ensure path: after a Patroni
// failover the slot may not exist on the new leader, so we
// recreate it with RESERVE_WAL so we can immediately read
// restart_lsn and compute the gap.
func CreatePhysicalSlotReserveWAL(ctx context.Context, c *pg.Conn, name string) error {
	if c == nil {
		return errors.New("replication: nil connection")
	}
	if c.Mode() != pg.ModeReplication {
		return output.NewError("usage.wrong_mode",
			"CreatePhysicalSlotReserveWAL requires ModeReplication; got "+c.Mode().String()).Wrap(output.ErrUsage)
	}
	if !pg.ValidIdentifier(name) {
		return fmt.Errorf("replication: slot name %q is not a valid PG identifier "+
			"(letter/underscore start, then [a-z0-9_], ≤63 chars)", name)
	}
	// Defence in depth: the slot name is interpolated unquoted into
	// the replication-protocol command below. ValidIdentifier above
	// rejects whitespace/quotes/control chars so the tokenizer can't
	// be steered to additional commands; PG's own 42602 only fires
	// after the wire has been written.
	q := fmt.Sprintf("CREATE_REPLICATION_SLOT %s PHYSICAL RESERVE_WAL", name)
	mrr := c.PgConn().Exec(ctx, q)
	_, err := mrr.ReadAll()
	if err != nil {
		if isDuplicateObjectErr(err) {
			return nil
		}
		return fmt.Errorf("replication: %s: %w", q, err)
	}
	return nil
}

// DropSlot removes the named replication slot. Idempotent: if the
// slot is already absent the call returns nil.
func DropSlot(ctx context.Context, c *pg.Conn, name string) error {
	if c == nil {
		return errors.New("replication: nil connection")
	}
	if c.Mode() != pg.ModeReplication {
		return output.NewError("usage.wrong_mode",
			"DropSlot requires ModeReplication; got "+c.Mode().String()).Wrap(output.ErrUsage)
	}
	if !pg.ValidIdentifier(name) {
		return fmt.Errorf("replication: slot name %q is not a valid PG identifier "+
			"(letter/underscore start, then [a-z0-9_], ≤63 chars)", name)
	}
	err := pglogrepl.DropReplicationSlot(ctx, c.PgConn(), name, pglogrepl.DropReplicationSlotOptions{Wait: true})
	if err != nil {
		if isUndefinedObjectErr(err) {
			return nil
		}
		return fmt.Errorf("replication: drop slot %q: %w", name, err)
	}
	return nil
}

// GetSlot returns the SlotInfo for name on a regular-mode connection
// by querying pg_replication_slots. Returns nil + ErrSlotMissing when
// the slot doesn't exist.
//
// Connection mode: ModeRegular (the SQL view isn't accessible from a
// replication-mode connection).
func GetSlot(ctx context.Context, c *pg.Conn, name string) (*SlotInfo, error) {
	if c == nil {
		return nil, errors.New("replication: nil connection")
	}
	if c.Mode() != pg.ModeRegular {
		return nil, output.NewError("usage.wrong_mode",
			"GetSlot requires ModeRegular; got "+c.Mode().String()).Wrap(output.ErrUsage)
	}
	const q = `SELECT slot_name, slot_type, active, restart_lsn::text, confirmed_flush_lsn::text, plugin
	           FROM pg_replication_slots WHERE slot_name = $1`
	res := c.PgConn().ExecParams(ctx, q, [][]byte{[]byte(name)}, nil, nil, nil).Read()
	if res.Err != nil {
		return nil, fmt.Errorf("replication: query pg_replication_slots: %w", res.Err)
	}
	if len(res.Rows) == 0 {
		return nil, ErrSlotMissing
	}
	row := res.Rows[0]
	if len(row) < 6 {
		return nil, fmt.Errorf("replication: pg_replication_slots returned %d columns", len(row))
	}
	info := &SlotInfo{
		Name:              string(row[0]),
		Type:              string(row[1]),
		Active:            string(row[2]) == "t",
		RestartLSN:        string(row[3]),
		ConfirmedFlushLSN: string(row[4]),
		Plugin:            string(row[5]),
	}
	return info, nil
}

// ErrSlotMissing is returned by GetSlot when the named slot doesn't exist.
var ErrSlotMissing = errors.New("replication: slot does not exist")

// isDuplicateObjectErr reports whether err carries SQLSTATE 42710
// (duplicate_object), which PG raises when CREATE_REPLICATION_SLOT
// targets a name that's already in use.
func isDuplicateObjectErr(err error) bool {
	return hasSQLSTATE(err, "42710")
}

// isUndefinedObjectErr reports whether err carries SQLSTATE 42704
// (undefined_object), raised when DROP_REPLICATION_SLOT targets a
// name that doesn't exist.
func isUndefinedObjectErr(err error) bool {
	return hasSQLSTATE(err, "42704")
}

// hasSQLSTATE walks the error chain looking for a *pgconn.PgError or
// our own *streaming.ServerError with the given SQLSTATE code.
func hasSQLSTATE(err error, code string) bool {
	type sqlstater interface {
		SQLState() string
	}
	for err != nil {
		if s, ok := err.(sqlstater); ok && s.SQLState() == code {
			return true
		}
		// Some chains expose a Code field via duck-typed types. We
		// don't want to import pgconn here (already linked
		// transitively); the SQLState() method is the canonical way
		// to ask, and pgconn.PgError implements it.
		err = errors.Unwrap(err)
	}
	return false
}
