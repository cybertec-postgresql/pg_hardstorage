//go:build integration

package partial_test

import (
	"context"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/partial"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	pgtestkit "github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
)

// TestLookupRelfilenodes_HappyPath spins up PG, creates a couple of
// tables (one with TOAST-able columns to exercise the TOAST mapping),
// and asserts the lookup returns sane relfilenode paths plus the
// TOAST companion entry.
func TestLookupRelfilenodes_HappyPath(t *testing.T) {
	pgInst := pgtestkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Create the test tables in a regular-mode connection.
	c, err := pg.Connect(ctx, pgInst.DSN, pg.ModeRegular)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close(ctx)

	stmts := []string{
		`CREATE SCHEMA tnt`,
		`CREATE TABLE tnt.users (id serial PRIMARY KEY, name text)`,
		// `bigtext` triggers TOAST automatically — text columns get
		// a TOAST table created when needed.
		`CREATE TABLE tnt.notes (id serial PRIMARY KEY, body text)`,
		`INSERT INTO tnt.notes(body) SELECT repeat('abc', 4000) FROM generate_series(1, 5)`,
	}
	for _, q := range stmts {
		res := c.PgConn().ExecParams(ctx, q, nil, nil, nil, nil).Read()
		if res.Err != nil {
			t.Fatalf("setup %q: %v", q, res.Err)
		}
	}

	rfns, err := partial.LookupRelfilenodes(ctx, partial.LookupOptions{
		PGConnString: pgInst.DSN,
		Tables:       []string{"tnt.users", "tnt.notes", "tnt.does_not_exist"},
	})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(rfns) != 3 {
		t.Fatalf("len = %d, want 3", len(rfns))
	}

	users := rfns[0]
	if users.Schema != "tnt" || users.Table != "users" {
		t.Errorf("users: %+v", users)
	}
	if users.NotFound {
		t.Error("users should be found")
	}
	if users.Path == "" || users.Relfilenode == 0 {
		t.Errorf("users path/relfilenode empty: %+v", users)
	}

	notes := rfns[1]
	if notes.Schema != "tnt" || notes.Table != "notes" {
		t.Errorf("notes: %+v", notes)
	}
	if notes.ToastOID == 0 || notes.ToastPath == "" {
		// notes has a text column with rows large enough to trigger
		// TOAST; the table should have a reltoastrelid pointing at
		// the TOAST relation.
		t.Errorf("notes should have a TOAST companion: %+v", notes)
	}

	missing := rfns[2]
	if !missing.NotFound {
		t.Errorf("does_not_exist should be NotFound: %+v", missing)
	}
	if missing.Qualified != "tnt.does_not_exist" {
		t.Errorf("missing.Qualified = %q", missing.Qualified)
	}
}
