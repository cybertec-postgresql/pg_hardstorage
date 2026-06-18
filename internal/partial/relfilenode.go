// Package partial provides table-level inspection + extraction
// helpers for the `pg_hardstorage partial` command tree.
//
// surface:
//
//   - Relfilenode lookup: given qualified table names + a live
//     source DB connection, return the on-disk file paths each
//     table's heap (and TOAST table, when present) lives at.
//     Combined with a manifest walk, this answers the operator's
//     "would my partial restore work, and how big is it?"
//     question without needing to actually do the restore.
//
// What's deliberately NOT here:
//
//   - The actual extraction path (sandbox PG + pg_dump) — that's
//     the larger piece. The CLI's `partial restore` command
//     still surfaces a structured workaround until the sandbox
//     extractor lands.
//   - Index files (we only map table heap data + TOAST). Indexes
//     are rebuildable from the heap, so a partial restore that
//     skips them is cheaper and the rebuild is the operator's
//     responsibility on the target DB.
package partial

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

// Relfilenode describes one table's on-disk presence in PG's data
// directory. Path is the relative path returned by
// pg_relation_filepath(); it's what shows up in our manifest
// FileEntries' Path field, so the operator can match against the
// manifest.
type Relfilenode struct {
	Schema      string `json:"schema"`
	Table       string `json:"table"`
	Qualified   string `json:"qualified"` // schema + "." + table
	OID         uint32 `json:"oid"`
	Relfilenode uint32 `json:"relfilenode"`
	Path        string `json:"path"`
	ToastOID    uint32 `json:"toast_oid,omitempty"`
	ToastPath   string `json:"toast_path,omitempty"`
	NotFound    bool   `json:"not_found,omitempty"` // table didn't exist in pg_class
}

// LookupOptions tunes LookupRelfilenodes.
type LookupOptions struct {
	// PGConnString is a libpq URI / DSN for the source database in
	// regular mode. The connecting user needs SELECT on pg_class +
	// pg_namespace, which the default `public` role already has.
	PGConnString string

	// Tables is the list of qualified names to look up.
	// Examples: "public.users", "tenant_1.events".
	// Unqualified names are rejected — qualifying explicitly
	// avoids ambiguity around search_path differences between the
	// agent and the target DB.
	Tables []string
}

// LookupRelfilenodes opens a regular-mode connection, queries pg_class
// + pg_namespace + pg_relation_filepath() for the requested tables,
// and returns one Relfilenode per requested name. Tables not present
// in pg_class get NotFound=true rather than aborting the call — the
// caller wants the partial-restore "would this work?" view to
// complete even when some names are typos.
func LookupRelfilenodes(ctx context.Context, opts LookupOptions) ([]Relfilenode, error) {
	if opts.PGConnString == "" {
		return nil, errors.New("partial: PGConnString is required")
	}
	if len(opts.Tables) == 0 {
		return nil, errors.New("partial: Tables is empty")
	}
	for _, t := range opts.Tables {
		if !strings.Contains(t, ".") {
			return nil, fmt.Errorf("partial: %q is unqualified; pass schema.table (e.g. public.users) for unambiguous lookup", t)
		}
	}

	c, err := pg.Connect(ctx, opts.PGConnString, pg.ModeRegular)
	if err != nil {
		return nil, fmt.Errorf("partial: connect: %w", err)
	}
	defer c.Close(ctx)

	out := make([]Relfilenode, 0, len(opts.Tables))
	for _, qualified := range opts.Tables {
		// Split on the FIRST dot — schemas can't contain dots, but
		// table names theoretically can if quoted (which we don't
		// support here; keep it simple).
		schema, table, ok := strings.Cut(qualified, ".")
		if !ok {
			// Already validated above, but defensive.
			out = append(out, Relfilenode{Qualified: qualified, NotFound: true})
			continue
		}
		rfn, err := lookupOne(ctx, c, schema, table)
		if err != nil {
			return out, fmt.Errorf("partial: lookup %q: %w", qualified, err)
		}
		rfn.Qualified = qualified
		if rfn.Schema == "" {
			// "Not found" path — preserve the requested name so the
			// operator sees what they asked for in the result.
			rfn.Schema = schema
			rfn.Table = table
		}
		out = append(out, rfn)
	}
	return out, nil
}

// lookupOne runs the per-table pg_class query. Returns a zero-value
// Relfilenode + nil error when the table isn't found (so the caller
// can mark NotFound without re-classifying transient errors as
// missing).
func lookupOne(ctx context.Context, c *pg.Conn, schema, table string) (Relfilenode, error) {
	const q = `
SELECT c.oid::int8,
       c.relfilenode::int8,
       pg_relation_filepath(c.oid),
       COALESCE(t.oid, 0)::int8,
       COALESCE(pg_relation_filepath(t.oid), '')
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_class t ON t.oid = c.reltoastrelid
WHERE n.nspname = $1 AND c.relname = $2
LIMIT 1`
	res := c.PgConn().ExecParams(ctx, q,
		[][]byte{[]byte(schema), []byte(table)},
		nil, nil, nil).Read()
	if res.Err != nil {
		return Relfilenode{}, fmt.Errorf("query pg_class: %w", res.Err)
	}
	if len(res.Rows) == 0 {
		return Relfilenode{NotFound: true}, nil
	}
	row := res.Rows[0]
	if len(row) != 5 {
		return Relfilenode{}, fmt.Errorf("query returned %d cols, want 5", len(row))
	}
	oid, err := parseUint32(row[0])
	if err != nil {
		return Relfilenode{}, fmt.Errorf("parse oid: %w", err)
	}
	rfn, err := parseUint32(row[1])
	if err != nil {
		return Relfilenode{}, fmt.Errorf("parse relfilenode: %w", err)
	}
	path := string(row[2])
	toastOID, _ := parseUint32(row[3])
	toastPath := string(row[4])
	return Relfilenode{
		Schema:      schema,
		Table:       table,
		OID:         oid,
		Relfilenode: rfn,
		Path:        path,
		ToastOID:    toastOID,
		ToastPath:   toastPath,
	}, nil
}

func parseUint32(b []byte) (uint32, error) {
	var n uint64
	if _, err := fmt.Sscan(string(b), &n); err != nil {
		return 0, err
	}
	return uint32(n), nil
}
