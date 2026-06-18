// Package logicalreceiver wraps PostgreSQL's logical-replication protocol
// for two purposes:
//
//   - Logical-slot management (Create / Drop / Get) via the
//     `pgoutput` built-in plugin.
//   - Continuous logical-record receive (Stream) over a persistent
//     logical replication slot.
//
// Architectural symmetry with internal/pg/replication:
//
//	replication/        — physical WAL slot + PHYSICAL replication
//	logicalreceiver/    — logical decoding slot + LOGICAL replication
//
// Both run on the same persistent-slot model — PG holds WAL until the
// slot's confirmed_flush_lsn advances, which we do via standby status
// updates after each batch is durably committed in the repo.
//
// Output plugin: v0.1 uses `pgoutput` exclusively. It's the in-tree PG
// plugin (no extension install required, works on managed PG that
// disallows custom plugins), produces a documented binary protocol,
// and pglogrepl already has decoders for every message type.
// `wal2json` and `pg_hardstorage_proto` plug in via the same Stream
// surface.
package logicalreceiver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

// DefaultPlugin is the output plugin v0.1 ships against. pgoutput is
// the canonical built-in; every PG since 10 has it without extra
// configuration.
const DefaultPlugin = "pgoutput"

// SlotInfo describes one logical replication slot. Field set covers
// the read columns the agent actually consumes; the full
// pg_replication_slots view has 14 columns, most of which are
// uninteresting at the agent layer.
type SlotInfo struct {
	Name              string
	Plugin            string
	Active            bool
	RestartLSN        string
	ConfirmedFlushLSN string
	Database          string
}

// CreateLogicalSlot creates a persistent logical replication slot on
// a replication-mode connection. Idempotent on SQLSTATE 42710
// (duplicate_object): re-running against an existing slot returns
// nil rather than an error, so `pg_hardstorage logical add` can be
// run repeatedly without churn.
//
// plugin defaults to DefaultPlugin (pgoutput) when empty.
func CreateLogicalSlot(ctx context.Context, c *pg.Conn, name, plugin string) error {
	if c == nil {
		return errors.New("logicalreceiver: nil connection")
	}
	if name == "" {
		return errors.New("logicalreceiver: empty slot name")
	}
	if plugin == "" {
		plugin = DefaultPlugin
	}
	_, err := pglogrepl.CreateReplicationSlot(ctx, c.PgConn(), name, plugin,
		pglogrepl.CreateReplicationSlotOptions{
			Mode: pglogrepl.LogicalReplication,
		})
	if err != nil {
		// Swallow duplicate-object the same way the physical-slot
		// helper does: the slot exists, that's the desired state,
		// we did our job.
		if isDuplicateObject(err) {
			return nil
		}
		return fmt.Errorf("logicalreceiver: create slot %q: %w", name, err)
	}
	return nil
}

// DropLogicalSlot drops a logical slot. WAIT=true in v0.1: we want
// errors to surface rather than silently disappearing on a busy
// slot.
func DropLogicalSlot(ctx context.Context, c *pg.Conn, name string) error {
	if c == nil {
		return errors.New("logicalreceiver: nil connection")
	}
	err := pglogrepl.DropReplicationSlot(ctx, c.PgConn(), name,
		pglogrepl.DropReplicationSlotOptions{Wait: true})
	if err != nil {
		return fmt.Errorf("logicalreceiver: drop slot %q: %w", name, err)
	}
	return nil
}

// isDuplicateObject reports whether err is PG's SQLSTATE 42710
// (duplicate_object). pgconn surfaces these as plain text-matched
// strings on Exec failures, so we look for the canonical message.
func isDuplicateObject(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "42710") ||
		strings.Contains(s, "already exists") ||
		strings.Contains(s, "ERROR: replication slot") && strings.Contains(s, "already exists")
}
