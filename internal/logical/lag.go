// lag.go — LagResult: per-slot lag/restart-LSN metrics computed from pg_replication_slots.
package logical

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
)

// LagResult is what `pg_hardstorage logical status <name>
// --pg-connection <url>` surfaces alongside the registry view.
//
// The numbers come from PG's pg_replication_slots view + a single
// pg_current_wal_lsn() call:
//
//	BehindBytes = current_wal_lsn - confirmed_flush_lsn
//	             // distance the agent's flush is behind the primary's
//	             // most recent WAL position. 0 when caught up.
//
//	RestartLSN  // PG's "I can't release WAL older than this" marker;
//	            // useful as the operator's "how much WAL is the slot
//	            // forcing PG to retain?" indicator.
//
//	Active      // whether some replication consumer is currently
//	            // attached to the slot. v0.1's logical pipeline holds
//	            // the slot for the duration of `logical stream`; if
//	            // Active is false outside that window, the agent
//	            // isn't currently consuming.
type LagResult struct {
	SlotName          string `json:"slot_name"`
	Plugin            string `json:"plugin"`
	Active            bool   `json:"active"`
	RestartLSN        string `json:"restart_lsn,omitempty"`
	ConfirmedFlushLSN string `json:"confirmed_flush_lsn,omitempty"`
	CurrentWALLSN     string `json:"current_wal_lsn,omitempty"`
	BehindBytes       int64  `json:"behind_bytes"`
}

// LagSchema is the on-disk version tag for LagResult bodies. Kept
// independent of the registry schema so a change to lag rendering
// doesn't have to migrate every Stream record.
const LagSchema = "pg_hardstorage.logical_lag.v1"

// ErrSlotNotFound surfaces when the named slot doesn't exist in
// pg_replication_slots. Distinct from the registry-side
// ErrStreamNotFound — the registry can hold a Stream whose slot
// hasn't been created yet (or got dropped behind the agent's back).
var ErrSlotNotFound = errors.New("logical: replication slot not found in pg_replication_slots")

// Lag opens a regular-mode connection to PG, queries the named slot,
// and computes the byte-distance lag against pg_current_wal_lsn().
// Returns ErrSlotNotFound when the slot is unknown to PG.
//
// We accept a libpq connection string rather than an open *pg.Conn
// so the caller doesn't have to worry about the regular-vs-
// replication mode split — Lag owns its own connection.
func Lag(ctx context.Context, pgConnString, slotName string) (*LagResult, error) {
	if slotName == "" {
		return nil, errors.New("logical: slot name is required")
	}
	if pgConnString == "" {
		return nil, errors.New("logical: pg connection string is required")
	}
	c, err := pg.Connect(ctx, pgConnString, pg.ModeRegular)
	if err != nil {
		return nil, fmt.Errorf("logical: connect: %w", err)
	}
	defer c.Close(ctx)

	info, err := replication.GetSlot(ctx, c, slotName)
	if err != nil {
		// Map "no rows" to our sentinel so callers can react.
		if errors.Is(err, replication.ErrSlotMissing) {
			return nil, ErrSlotNotFound
		}
		return nil, fmt.Errorf("logical: get slot: %w", err)
	}

	curr, err := currentWALLSN(ctx, c)
	if err != nil {
		return nil, err
	}

	res := &LagResult{
		SlotName:          info.Name,
		Plugin:            info.Plugin,
		Active:            info.Active,
		RestartLSN:        info.RestartLSN,
		ConfirmedFlushLSN: info.ConfirmedFlushLSN,
		CurrentWALLSN:     curr.String(),
	}
	// Compute byte distance only when we have both LSNs — a
	// just-created slot has empty ConfirmedFlushLSN until first
	// flush.
	if info.ConfirmedFlushLSN != "" {
		flushed, err := pglogrepl.ParseLSN(info.ConfirmedFlushLSN)
		if err != nil {
			return nil, fmt.Errorf("logical: parse confirmed_flush_lsn %q: %w",
				info.ConfirmedFlushLSN, err)
		}
		// pglogrepl.LSN is uint64; signed math would overflow on PG
		// rewind, but a behind-the-current never goes negative.
		// Clamp at 0 to avoid surprises on the rare-but-possible
		// "consumer ahead of primary" case (e.g. clock skew with
		// the LSN cache).
		if curr >= flushed {
			res.BehindBytes = int64(curr - flushed)
		}
	}
	return res, nil
}

// currentWALLSN returns pg_current_wal_lsn() as an LSN. Standalone
// helper so a future enhancement (read pg_last_wal_replay_lsn() on
// a standby) doesn't have to fork this code path.
func currentWALLSN(ctx context.Context, c *pg.Conn) (pglogrepl.LSN, error) {
	res := c.PgConn().ExecParams(ctx, `SELECT pg_current_wal_lsn()::text`, nil, nil, nil, nil).Read()
	if res.Err != nil {
		return 0, fmt.Errorf("logical: pg_current_wal_lsn: %w", res.Err)
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
		return 0, errors.New("logical: pg_current_wal_lsn returned no rows")
	}
	lsn, err := pglogrepl.ParseLSN(string(res.Rows[0][0]))
	if err != nil {
		return 0, fmt.Errorf("logical: parse current wal lsn %q: %w",
			string(res.Rows[0][0]), err)
	}
	return lsn, nil
}
