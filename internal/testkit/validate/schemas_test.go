package validate_test

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/validate"
)

// recordingExec captures every SQL statement issued by a schema
// so tests can assert on the shape of the workload without a
// real PostgreSQL.
type recordingExec struct {
	stmts [][]any
}

func (r *recordingExec) fn() validate.ExecFn {
	return func(sql string, args ...any) error {
		row := []any{sql}
		row = append(row, args...)
		r.stmts = append(r.stmts, row)
		return nil
	}
}

func TestLookupSchema_Known(t *testing.T) {
	for _, name := range []string{"tpcc-lite", "bulk-copy", "schema-churn", ""} {
		s, err := validate.LookupSchema(name)
		if err != nil {
			t.Errorf("%q: unexpected err: %v", name, err)
		}
		if s == nil {
			t.Errorf("%q: nil schema", name)
		}
	}
}

func TestLookupSchema_Unknown(t *testing.T) {
	if _, err := validate.LookupSchema("nope"); err == nil {
		t.Errorf("expected unknown-schema error")
	}
}

func TestTpccLite_Setup(t *testing.T) {
	s, _ := validate.LookupSchema("tpcc-lite")
	rec := &recordingExec{}
	if err := s.Setup(rec.fn()); err != nil {
		t.Fatal(err)
	}
	// Expect customers, orders, order_line, plus the seed
	// insert — 4 statements.
	if len(rec.stmts) != 4 {
		t.Fatalf("expected 4 setup stmts; got %d", len(rec.stmts))
	}
	if !strings.Contains(rec.stmts[0][0].(string), "customers") {
		t.Errorf("first stmt should create customers; got %s", rec.stmts[0][0])
	}
}

func TestTpccLite_IterationPropagatesArgs(t *testing.T) {
	s, _ := validate.LookupSchema("tpcc-lite")
	rec := &recordingExec{}
	rng := rand.New(rand.NewSource(42))
	bytes, err := s.Iteration(rec.fn(), rng, 100)
	if err != nil {
		t.Fatal(err)
	}
	if bytes <= 0 {
		t.Errorf("expected non-zero churn; got %d", bytes)
	}
	// Should issue INSERTs and UPDATEs, both with 2 args.
	hasInsert := false
	hasUpdate := false
	for _, stmt := range rec.stmts {
		s := stmt[0].(string)
		if strings.HasPrefix(s, "INSERT INTO orders") {
			if len(stmt) != 3 {
				t.Errorf("INSERT INTO orders should have 2 args; got %d", len(stmt)-1)
			}
			hasInsert = true
		}
		if strings.HasPrefix(s, "UPDATE customers") {
			hasUpdate = true
		}
	}
	if !hasInsert {
		t.Errorf("expected INSERT INTO orders")
	}
	if !hasUpdate {
		t.Errorf("expected UPDATE customers")
	}
}

func TestBulkCopy_OneInsertPerIteration(t *testing.T) {
	s, _ := validate.LookupSchema("bulk-copy")
	rec := &recordingExec{}
	rng := rand.New(rand.NewSource(1))
	if _, err := s.Iteration(rec.fn(), rng, 100); err != nil {
		t.Fatal(err)
	}
	if len(rec.stmts) != 1 {
		t.Errorf("bulk-copy should issue exactly one INSERT...SELECT; got %d", len(rec.stmts))
	}
	if !strings.Contains(rec.stmts[0][0].(string), "generate_series") {
		t.Errorf("bulk-copy iteration should use generate_series; got %s", rec.stmts[0][0])
	}
}

func TestBulkCopy_RowsScaleWithChurn(t *testing.T) {
	// Higher churn should produce a larger N argument to
	// generate_series.
	s, _ := validate.LookupSchema("bulk-copy")
	low := &recordingExec{}
	high := &recordingExec{}
	rng := rand.New(rand.NewSource(1))
	_, _ = s.Iteration(low.fn(), rng, 10)
	_, _ = s.Iteration(high.fn(), rng, 1000)
	lowN := low.stmts[0][1].(int)
	highN := high.stmts[0][1].(int)
	if highN <= lowN {
		t.Errorf("higher churn should produce more rows: lowN=%d highN=%d", lowN, highN)
	}
}

func TestSchemaChurn_AltersAfterFiveIterations(t *testing.T) {
	s, _ := validate.LookupSchema("schema-churn")
	rec := &recordingExec{}
	rng := rand.New(rand.NewSource(99))
	for i := 0; i < 6; i++ {
		if _, err := s.Iteration(rec.fn(), rng, 50); err != nil {
			t.Fatal(err)
		}
	}
	hasAlter := false
	for _, stmt := range rec.stmts {
		if strings.Contains(stmt[0].(string), "ALTER TABLE customers ADD COLUMN") {
			hasAlter = true
		}
	}
	if !hasAlter {
		t.Errorf("expected at least one ADD COLUMN after 6 iterations")
	}
}
