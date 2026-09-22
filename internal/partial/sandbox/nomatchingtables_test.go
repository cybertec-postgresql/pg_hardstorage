package sandbox

import (
	"errors"
	"fmt"
	"testing"
)

// pg_dump reports "your --table pattern matched nothing" by exiting 1
// with a diagnostic on stderr — the same exit status it uses for a
// refused connection, a permission denial and a corrupt catalog. Exit
// status alone therefore cannot separate "the table is not here" from
// "the dump failed", and the two need different exit codes and
// different advice from pg_hardstorage.
//
// This mattered more than it looks. `partial dump` carried an
// empty-dump guard for exactly this case, written against pg_dump
// builds that exit 0 and emit nothing. Current builds do not: they
// error, the error path returns first, and the guard was unreachable
// on every modern PostgreSQL. The scenario that would have caught it
// (L3_partial_dump_database_flag) skips unless the host has server
// binaries, so it never ran.

func TestIsNoMatchingTables(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   bool
	}{
		{
			name:   "the real diagnostic, verbatim from pg_dump 17.11",
			stderr: "pg_dump: error: no matching tables were found",
			want:   true,
		},
		{
			name:   "case-insensitive, in case the wording is ever capitalised",
			stderr: "pg_dump: error: No matching tables were found",
			want:   true,
		},
		{
			name:   "with surrounding noise, as tailString may leave it",
			stderr: "pg_dump: warning: something\npg_dump: error: no matching tables were found\n",
			want:   true,
		},
		{
			name:   "a genuine dump failure must NOT be softened into not-found",
			stderr: "pg_dump: error: connection to server on socket \"/tmp/.s.PGSQL.5432\" failed",
			want:   false,
		},
		{
			name:   "permission denial is a fault, not an absence",
			stderr: "pg_dump: error: query failed: ERROR:  permission denied for table accounts",
			want:   false,
		},
		{
			name:   "a matching-schema message is a different thing entirely",
			stderr: "pg_dump: error: no matching schemas were found",
			want:   false,
		},
		{name: "empty stderr", stderr: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNoMatchingTables(tc.stderr); got != tc.want {
				t.Errorf("isNoMatchingTables(%q) = %v, want %v", tc.stderr, got, tc.want)
			}
		})
	}
}

// TestErrNoMatchingTablesIsIdentifiable pins the sentinel's contract:
// the CLI routes on errors.Is, so wrapping must preserve identity and
// the wrapped text must still carry pg_dump's own words for the
// operator.
func TestErrNoMatchingTablesIsIdentifiable(t *testing.T) {
	wrapped := fmt.Errorf("%w: %s", ErrNoMatchingTables, "pg_dump: error: no matching tables were found")
	if !errors.Is(wrapped, ErrNoMatchingTables) {
		t.Fatal("wrapping broke errors.Is; the CLI cannot distinguish not-found from a dump failure")
	}
	if errors.Is(errors.New("some other failure"), ErrNoMatchingTables) {
		t.Fatal("unrelated errors must not match the sentinel")
	}
}
