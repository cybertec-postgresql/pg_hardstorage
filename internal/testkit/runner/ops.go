// ops.go — testkit load operation dispatchers (create_table, insert, update, …) safe by named schemas.
package runner

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/load"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/load/prng"
)

// runCreateTable implements `create_table`.
//
// We support a small set of named schemas. A production testkit would
// allow arbitrary CREATE TABLE strings, but that opens an injection
// surface we don't want from operator-supplied YAML. The named-schema
// approach keeps load files tiny and reproducible; new schemas land
// as Go code reviewed in the same way as the rest of the testkit.
func runCreateTable(ctx context.Context, db *sql.DB, op load.Operation) error {
	if op.Name == "" {
		return fmt.Errorf("create_table: name is required")
	}
	schema := op.Schema
	if schema == "" {
		// Default to a generic schema if none specified.
		schema = "users_v1"
	}
	ddl, ok := tableDDL(op.Name, schema)
	if !ok {
		return fmt.Errorf("create_table: unknown schema %q (v0.1 ships users_v1, orders_v1, events_v1)", schema)
	}
	_, err := db.ExecContext(ctx, ddl)
	return err
}

func tableDDL(name, schema string) (string, bool) {
	switch schema {
	case "users_v1":
		return fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS %s (
				id        bigserial PRIMARY KEY,
				email     text NOT NULL,
				full_name text,
				created_at timestamptz NOT NULL DEFAULT now()
			)`, identifierSafe(name)), true
	case "orders_v1":
		return fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS %s (
				id         bigserial PRIMARY KEY,
				user_id    bigint NOT NULL,
				amount_cents bigint NOT NULL,
				created_at timestamptz NOT NULL DEFAULT now()
			)`, identifierSafe(name)), true
	case "events_v1":
		return fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS %s (
				id         bigserial PRIMARY KEY,
				kind       text NOT NULL,
				payload    jsonb NOT NULL,
				created_at timestamptz NOT NULL DEFAULT now()
			)`, identifierSafe(name)), true
	}
	return "", false
}

// runInsertRows implements `insert_rows` deterministically. The PRNG is
// derived from the table name so two phases that hit different tables
// don't entangle their streams.
func runInsertRows(ctx context.Context, db *sql.DB, op load.Operation, out io.Writer) error {
	if op.Table == "" || op.Count <= 0 {
		return fmt.Errorf("insert_rows: want {table, count>0}")
	}
	gen := op.Generator
	if gen == "" {
		gen = "faker_users"
	}
	rng := prng.Derive(0xCAFE_BABE_DEAD_BEEF, op.Table)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	const batch = 1000
	stmt, err := tx.PrepareContext(ctx, insertStmt(op.Table, gen))
	if err != nil {
		return err
	}
	defer stmt.Close()

	emit(out, "load.insert.starting", map[string]any{
		"table":        op.Table,
		"count":        op.Count,
		"generator":    gen,
		"start_offset": op.StartOffset,
	})

	for i := 0; i < op.Count; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Index axis: caller-provided StartOffset shifts the
		// deterministic row-number space so a second
		// insert_rows call into the same table doesn't
		// regenerate identical-keyed rows (which would
		// duplicate-key-conflict against any unique index
		// added between the two calls).  See the
		// StartOffset doc on load.Operation for the canonical
		// shape.
		row, err := genRow(gen, rng, i+op.StartOffset)
		if err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, row...); err != nil {
			return fmt.Errorf("insert row %d: %w", i, err)
		}
		if i > 0 && i%batch == 0 {
			emit(out, "load.insert.progress", map[string]any{"table": op.Table, "rows": i})
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func insertStmt(table, gen string) string {
	switch gen {
	case "faker_users":
		return fmt.Sprintf("INSERT INTO %s (email, full_name) VALUES ($1, $2)", identifierSafe(table))
	case "faker_orders":
		return fmt.Sprintf("INSERT INTO %s (user_id, amount_cents) VALUES ($1, $2)", identifierSafe(table))
	case "faker_events":
		return fmt.Sprintf("INSERT INTO %s (kind, payload) VALUES ($1, $2::jsonb)", identifierSafe(table))
	}
	return ""
}

// genRow produces one row's value tuple deterministically from the PRNG.
// Generators are tiny — the role of the testkit isn't to test PG with
// realistic data, it's to drive enough byte volume that the chunker
// and CAS exercise their normal paths. Arbitrary stable bytes do that.
func genRow(gen string, rng interface{ Uint64() uint64 }, i int) ([]any, error) {
	switch gen {
	case "faker_users":
		return []any{
			fmt.Sprintf("user-%d@example.test", i),
			fmt.Sprintf("User %d (seed=%016x)", i, rng.Uint64()),
		}, nil
	case "faker_orders":
		return []any{
			int64(rng.Uint64() % 1_000_000), // user_id
			int64(rng.Uint64() % 1_000_000), // amount_cents
		}, nil
	case "faker_events":
		payload := fmt.Sprintf(`{"i":%d,"r":%d}`, i, rng.Uint64())
		return []any{"insert", payload}, nil
	}
	return nil, fmt.Errorf("unknown generator %q", gen)
}

// runCreateIndex builds a btree index. UNIQUE optional.
func runCreateIndex(ctx context.Context, db *sql.DB, op load.Operation) error {
	if op.Table == "" || len(op.Columns) == 0 {
		return fmt.Errorf("create_index: want {table, columns}")
	}
	cols := make([]string, len(op.Columns))
	for i, c := range op.Columns {
		cols[i] = identifierSafe(c)
	}
	uniq := ""
	if op.Unique {
		uniq = "UNIQUE "
	}
	idxName := fmt.Sprintf("idx_%s_%s", identifierSafe(op.Table), strings.Join(cols, "_"))
	stmt := fmt.Sprintf("CREATE %sINDEX IF NOT EXISTS %s ON %s (%s)",
		uniq, idxName, identifierSafe(op.Table), strings.Join(cols, ", "))
	_, err := db.ExecContext(ctx, stmt)
	return err
}

// runVacuum runs VACUUM (FULL?) on the named table.
func runVacuum(ctx context.Context, db *sql.DB, op load.Operation) error {
	tbl := op.Table
	if tbl == "" {
		_, err := db.ExecContext(ctx, "VACUUM")
		return err
	}
	full := ""
	if op.Full {
		full = "FULL "
	}
	_, err := db.ExecContext(ctx, "VACUUM "+full+identifierSafe(tbl))
	return err
}

// identifierSafe is the same allowlist as assert.identifierSafe.
// We duplicate the function (single-source-of-truth purists may
// dispute) to keep the assert and runner packages independent — they
// call into PG with operator-supplied names from different parsing
// layers and the safe-identifier rules are simple enough that
// duplication is cheaper than a shared helper.
func identifierSafe(s string) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9':
			out = append(out, byte(r))
		case r == '_', r == '.':
			out = append(out, byte(r))
		}
	}
	return strings.Trim(string(out), ".")
}
