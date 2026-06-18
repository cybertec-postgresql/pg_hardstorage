// Package pg is the PostgreSQL client surface. It wraps pgx/v5's pgconn
// in a thin, opinionated API tuned for backup operations:
//
//   - Two connection flavours — regular and replication — chosen by the
//     spec's "WAL via the replication protocol, not URLs" decision.
//   - Cancellation everywhere via context.Context.
//   - Error mapping into structured *output.Error so the dispatcher can
//     route exit codes correctly.
//
// Higher-level operations (BASE_BACKUP, START_REPLICATION, version
// probe) live in sibling packages: internal/pg/basebackup,
// internal/pg/walreceiver, etc.
package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// DefaultConnectTimeout is applied to PG connections when the
// operator's DSN doesn't carry an explicit `connect_timeout`. libpq's
// own default is 0 (infinite); we'd rather fail loud than block the
// agent indefinitely on a network partition.
//
// 30s is conservative-but-not-paranoid: long enough that a stressed
// replica responding slowly to TCP gets through, short enough that a
// black-hole'd subnet surfaces within an operator's normal
// scheduled-job latency budget.
const DefaultConnectTimeout = 30 * time.Second

// Mode selects the wire-protocol mode of a connection.
//
// PostgreSQL exposes two protocol modes over the same TCP port:
//   - Regular: SQL queries, the protocol everything-else uses.
//   - Replication: a small command vocabulary including IDENTIFY_SYSTEM,
//     BASE_BACKUP, START_REPLICATION; cannot run arbitrary SQL.
//
// We connect in the right mode for the operation. A backup uses
// Replication mode for BASE_BACKUP and START_REPLICATION; a probe
// uses Regular for SELECT version().
type Mode int

const (
	// ModeRegular is the standard SQL protocol mode.
	ModeRegular Mode = iota
	// ModeReplication is the streaming-replication protocol mode.
	ModeReplication
)

// String returns the canonical name of the mode.
func (m Mode) String() string {
	switch m {
	case ModeRegular:
		return "regular"
	case ModeReplication:
		return "replication"
	}
	return fmt.Sprintf("Mode(%d)", int(m))
}

// Conn is a thin wrapper around *pgconn.PgConn that records the mode it
// was opened in and centralises error mapping.
type Conn struct {
	pg   *pgconn.PgConn
	mode Mode
}

// PgConn returns the underlying pgx connection. Callers that need to
// reach a specific pgx primitive (e.g. CopyTo, Exec, the byte-level
// frontend) use this; the wrapper doesn't try to abstract the entire
// pgx surface.
func (c *Conn) PgConn() *pgconn.PgConn { return c.pg }

// Mode reports the mode this connection was opened in.
func (c *Conn) Mode() Mode { return c.mode }

// ConnectOption tunes a Connect call after the DSN is parsed.
type ConnectOption func(*pgconn.Config)

// WithApplicationName sets the connection's application_name — the
// string PostgreSQL records in pg_stat_activity / pg_stat_replication
// and matches against synchronous_standby_names. The WAL streamer
// pins it so an operator can name pg_hardstorage as a (would-be)
// synchronous standby and the preflight can detect that.
func WithApplicationName(name string) ConnectOption {
	return func(cfg *pgconn.Config) {
		if name == "" {
			return
		}
		if cfg.RuntimeParams == nil {
			cfg.RuntimeParams = map[string]string{}
		}
		cfg.RuntimeParams["application_name"] = name
	}
}

// Connect opens a connection to PostgreSQL using a libpq-style URL or
// keyword/value DSN. mode chooses between regular SQL and the
// streaming-replication protocol; it is mapped to the connection-string
// parameter PostgreSQL expects (replication=database for replication
// mode). When the DSN omits connect_timeout, DefaultConnectTimeout is
// applied so a black-holed network surfaces promptly. Returns a
// structured *output.Error on failure so the CLI maps to the right
// exit code (storage.unreachable / auth.denied / ...).
func Connect(ctx context.Context, dsn string, mode Mode, opts ...ConnectOption) (*Conn, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return nil, output.NewError("usage.bad_pg_dsn",
			fmt.Sprintf("parse PG connection string: %v", err)).
			Wrap(output.ErrUsage)
	}
	// libpq's connect_timeout defaults to 0 (infinite). Without an
	// explicit timeout, certain TCP failure modes (SYN-sent with no
	// RST/ACK, half-open connections after a network partition) can
	// hang the call indefinitely — even with a cancellable ctx,
	// because pgconn.ConnectConfig only checks ctx between dial
	// stages. Set a sensible upper bound here when the operator's
	// DSN didn't.
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = DefaultConnectTimeout
	}
	switch mode {
	case ModeReplication:
		// Setting RuntimeParams[replication] = database puts the
		// connection into replication-protocol mode. This is the same
		// effect as `replication=database` in the libpq DSN.
		if cfg.RuntimeParams == nil {
			cfg.RuntimeParams = map[string]string{}
		}
		cfg.RuntimeParams["replication"] = "database"
	case ModeRegular:
		// Make sure no leftover replication marker leaks in (pgx
		// preserves whatever was in the DSN otherwise).  The
		// case was accidentally deleted in ec3107d (security
		// audit out-of-scope edit), which made every regular-mode
		// caller — including the backup runner's probeVersion —
		// fail with "unknown connection mode regular".  Restored.
		delete(cfg.RuntimeParams, "replication")
	default:
		return nil, output.NewError("usage.bad_pg_mode",
			fmt.Sprintf("unknown connection mode %s", mode)).Wrap(output.ErrUsage)
	}

	for _, o := range opts {
		o(cfg)
	}

	pg, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, classifyConnectError(err)
	}
	return &Conn{pg: pg, mode: mode}, nil
}

// Close terminates the connection. Safe to call on a nil receiver and
// on an already-closed connection.
func (c *Conn) Close(ctx context.Context) error {
	if c == nil || c.pg == nil {
		return nil
	}
	return c.pg.Close(ctx)
}

// Ping checks the connection is alive with a no-op exchange. Useful as
// a doctor health-check primitive and as a pre-flight before issuing
// long-running commands.
func (c *Conn) Ping(ctx context.Context) error {
	if c == nil || c.pg == nil {
		return errors.New("pg: nil connection")
	}
	if err := c.pg.Ping(ctx); err != nil {
		return classifyConnectError(err)
	}
	return nil
}

// classifyConnectError maps pgx / network errors onto our structured
// error taxonomy. Most failures fall into two buckets the CLI cares
// about: unreachable backend (network / DNS / port closed) and auth
// failure (password, role, pg_hba). Other errors stay generic.
func classifyConnectError(err error) error {
	if err == nil {
		return nil
	}
	// pgconn surfaces server-side errors via *pgconn.PgError, with
	// SQLSTATE 28xxx for authentication-class failures.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "28000" || pgErr.Code == "28P01":
			return output.NewError("auth.denied",
				fmt.Sprintf("PG authentication failed: %s", pgErr.Message)).
				WithSuggestion(&output.Suggestion{
					Human: "check PG role / password / pg_hba.conf entry",
				}).Wrap(err)
		case pgErr.Code == "53300":
			return output.NewError("conflict.too_many_connections",
				fmt.Sprintf("PG refused: %s", pgErr.Message)).Wrap(err)
		}
	}
	// Network-side failures (no TCP, DNS, connection refused, timeout):
	// pgx wraps them as a *net.OpError or similar. We can't reliably
	// distinguish refused-vs-timeout without unwrapping further; treat
	// them all as "unreachable" so the CLI exits with code 8.
	return output.NewError("storage.unreachable",
		fmt.Sprintf("cannot connect to PostgreSQL: %v", err)).
		WithSuggestion(&output.Suggestion{
			Human: "verify host, port, network, and that PostgreSQL is accepting connections",
		}).Wrap(err)
}
