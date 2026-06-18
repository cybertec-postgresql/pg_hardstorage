package assert_test

import (
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/assert"
)

func TestParseList_AllShapes(t *testing.T) {
	body := []byte(`
- count_exact:    { table: users, value: 100 }
- count_range:    { table: orders, min: 80, max: 120 }
- lsn_at_least:   "0/12345678"
- audit_chain_intact: true
- pg_amcheck:     { passes: true }
- sql:
    query: "SELECT count(*) FROM x"
    expected: { rows: [[1]] }
`)
	var node yaml.Node
	if err := yaml.Unmarshal(body, &node); err != nil {
		t.Fatal(err)
	}
	// node is a document; descend to the sequence.
	seq := node.Content[0]
	list, err := assert.ParseList(seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 6 {
		t.Fatalf("want 6 assertions; got %d", len(list))
	}
	wantKinds := []string{"count_exact", "count_range", "lsn_at_least", "audit_chain_intact", "pg_amcheck", "sql"}
	for i, k := range wantKinds {
		if list[i].Kind != k {
			t.Errorf("list[%d].Kind = %q; want %q", i, list[i].Kind, k)
		}
	}
}

func TestRun_NoDB_SurfacesAsFailure(t *testing.T) {
	r := assert.Run(t.Context(), assert.Context{}, assert.Assertion{
		Kind: "count_exact",
		Args: map[string]any{"table": "users", "value": 100},
	})
	if r.Passed {
		t.Error("count_exact with no DB should fail")
	}
}

func TestRunAll_ReportsFailedCount(t *testing.T) {
	results, err := assert.RunAll(t.Context(), assert.Context{}, []assert.Assertion{
		{Kind: "count_exact", Args: map[string]any{"table": "x", "value": 1}},
		{Kind: "audit_chain_intact", Args: true},
	})
	if err == nil {
		t.Fatal("RunAll should report failure when at least one assertion fails")
	}
	if len(results) != 2 {
		t.Errorf("want 2 results; got %d", len(results))
	}
}
