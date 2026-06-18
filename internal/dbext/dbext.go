// Package dbext is the in-database extension surface.
//
// It ships the SQL the operator's PostgreSQL server needs to
// expose `pg_hardstorage.backups`, `pg_hardstorage.health` and
// `pg_hardstorage.rpo` views, plus the upsert helpers the agent
// uses to refresh them.
//
// Two modes:
//
//   - On-disk install: an operator with `make install` rights
//     copies the extension files (control + .sql) into the PG
//     server's `extension/` directory and runs `CREATE
//     EXTENSION pg_hardstorage`.  This is the standard
//     PostgreSQL extension flow.
//
//   - Inline install: `pg_hardstorage db install-extension
//     --pg-connection ...` runs the SQL the extension would
//     have installed, directly against the target database.
//     Useful in environments where `pg_config --pkglibdir`
//     isn't writable (managed PG, CI sandboxes).  No
//     `CREATE EXTENSION` row appears in pg_extension; the
//     schema + tables + views + functions still exist.
//
// Why ship both: managed PG (RDS, Cloud SQL, Supabase) refuses
// custom extensions but allows arbitrary CREATE TABLE / CREATE
// FUNCTION / CREATE VIEW.  The inline install is the only path
// that reaches those environments.
package dbext

import (
	_ "embed"
	"strings"
)

//go:embed schema.sql
var inlineSQL string

// InlineSQL returns the SQL string the inline-install path
// runs.  The bytes are the same DDL the on-disk extension's
// `pg_hardstorage--1.0.sql` ships, with the leading
// `\echo ... \quit` directive stripped (psql-only) so we can
// hand the body to a libpq Exec.
func InlineSQL() string {
	// The on-disk file's first line is a `\echo ... \quit`
	// guard for accidental psql invocations; strip it for the
	// inline path.
	out := inlineSQL
	if i := strings.Index(out, "\n"); i >= 0 && strings.HasPrefix(out, "\\echo") {
		out = out[i+1:]
	}
	return out
}

// SchemaName is the SQL schema the extension creates.  Stable
// across versions; bumping it would be a breaking change for
// every consumer query.
const SchemaName = "pg_hardstorage"

// ViewNames lists the operator-facing views the extension
// exposes.  Stable across the v1 schema commitment; new views
// land in v2 only.
var ViewNames = []string{"backups", "health", "rpo"}

// Version is the extension's SemVer string.  Encoded into the
// extension's `default_version` and into the SQL filename.
const Version = "1.0"
