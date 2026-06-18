package load_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/load"
)

func TestParse_Minimal(t *testing.T) {
	body := []byte(`
schema: pg_hardstorage.testload.v1
seed: 0xC0FFEE
phases:
  - name: bootstrap
    operations:
      - create_table:
          name: users
          schema: users_v1
      - insert_rows:
          table: users
          count: 100
          generator: faker_users
`)
	l, err := load.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if l.Seed != 0xC0FFEE {
		t.Errorf("Seed = %d", l.Seed)
	}
	if len(l.Phases) != 1 {
		t.Fatalf("want 1 phase; got %d", len(l.Phases))
	}
	if len(l.Phases[0].Operations) != 2 {
		t.Fatalf("want 2 operations; got %d", len(l.Phases[0].Operations))
	}
	op := l.Phases[0].Operations[0]
	if op.Kind != "create_table" {
		t.Errorf("kind = %q", op.Kind)
	}
	if op.Name != "users" || op.Schema != "users_v1" {
		t.Errorf("create_table fields wrong: %+v", op)
	}
}

func TestParse_RejectsUnknownSchema(t *testing.T) {
	body := []byte(`
schema: not.a.real.schema
seed: 1
phases: [{ name: x, operations: [] }]
`)
	_, err := load.Parse(body)
	if err == nil || !strings.Contains(err.Error(), "schema") {
		t.Errorf("expected schema-rejection error; got %v", err)
	}
}

func TestParse_RejectsZeroSeed(t *testing.T) {
	body := []byte(`
schema: pg_hardstorage.testload.v1
seed: 0
phases: [{ name: x, operations: [] }]
`)
	_, err := load.Parse(body)
	if err == nil || !strings.Contains(err.Error(), "seed") {
		t.Errorf("expected seed-rejection error; got %v", err)
	}
}

func TestParse_RejectsUnknownField(t *testing.T) {
	body := []byte(`
schema: pg_hardstorage.testload.v1
seed: 1
phases: [{ name: x, operations: [] }]
mystery_field: 42
`)
	_, err := load.Parse(body)
	if err == nil {
		t.Error("expected unknown-field error")
	}
}
