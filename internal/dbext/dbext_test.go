package dbext_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/dbext"
)

// TestInlineSQL_StripsPsqlGuard: the embedded SQL begins with
// `\echo ... \quit` (a psql guard against direct file
// execution).  InlineSQL must strip that line so a libpq
// Exec doesn't fail on the unknown command.
func TestInlineSQL_StripsPsqlGuard(t *testing.T) {
	got := dbext.InlineSQL()
	if strings.HasPrefix(got, `\echo`) {
		t.Errorf("InlineSQL should strip the \\echo guard, but starts with: %s",
			firstLine(got))
	}
}

// TestInlineSQL_ContainsCoreSchemaObjects: every view + every
// upsert function the SPEC commits to must appear in the
// embedded body.
func TestInlineSQL_ContainsCoreSchemaObjects(t *testing.T) {
	got := dbext.InlineSQL()
	wantSubstrings := []string{
		"CREATE TABLE pg_hardstorage.backups_state",
		"CREATE TABLE pg_hardstorage.health_state",
		"CREATE TABLE pg_hardstorage.rpo_state",
		"CREATE OR REPLACE VIEW pg_hardstorage.backups",
		"CREATE OR REPLACE VIEW pg_hardstorage.health",
		"CREATE OR REPLACE VIEW pg_hardstorage.rpo",
		"CREATE OR REPLACE FUNCTION pg_hardstorage.upsert_backup",
		"CREATE OR REPLACE FUNCTION pg_hardstorage.upsert_health",
		"CREATE OR REPLACE FUNCTION pg_hardstorage.upsert_rpo",
		"GRANT SELECT ON pg_hardstorage.backups TO PUBLIC",
		"GRANT SELECT ON pg_hardstorage.health  TO PUBLIC",
		"GRANT SELECT ON pg_hardstorage.rpo     TO PUBLIC",
		"pg_hardstorage_writer",
	}
	for _, w := range wantSubstrings {
		if !strings.Contains(got, w) {
			t.Errorf("InlineSQL missing %q", w)
		}
	}
}

// TestSchemaConstants: the package-level identifiers that
// downstream code keys off don't drift accidentally.
func TestSchemaConstants(t *testing.T) {
	if dbext.SchemaName != "pg_hardstorage" {
		t.Errorf("SchemaName drifted: %q", dbext.SchemaName)
	}
	if dbext.Version == "" {
		t.Error("Version is empty")
	}
	want := map[string]bool{"backups": false, "health": false, "rpo": false}
	for _, v := range dbext.ViewNames {
		if _, ok := want[v]; !ok {
			t.Errorf("unexpected view name %q", v)
		}
		want[v] = true
	}
	for v, found := range want {
		if !found {
			t.Errorf("expected view %q in ViewNames", v)
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
