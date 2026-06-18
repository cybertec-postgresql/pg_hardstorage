package partial_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/partial"
)

func TestLookupRelfilenodes_RequiresPGConn(t *testing.T) {
	_, err := partial.LookupRelfilenodes(context.Background(), partial.LookupOptions{
		Tables: []string{"public.users"},
	})
	if err == nil || !strings.Contains(err.Error(), "PGConnString") {
		t.Errorf("expected PGConnString error; got %v", err)
	}
}

func TestLookupRelfilenodes_RequiresTables(t *testing.T) {
	_, err := partial.LookupRelfilenodes(context.Background(), partial.LookupOptions{
		PGConnString: "postgres://localhost/x",
	})
	if err == nil || !strings.Contains(err.Error(), "Tables") {
		t.Errorf("expected Tables error; got %v", err)
	}
}

// TestLookupRelfilenodes_RefusesUnqualified asserts a clear error for
// the operator's most common mistake: passing a bare table name. We
// require schema.table to avoid silent search_path-dependent
// resolution.
func TestLookupRelfilenodes_RefusesUnqualified(t *testing.T) {
	_, err := partial.LookupRelfilenodes(context.Background(), partial.LookupOptions{
		PGConnString: "postgres://localhost/x",
		Tables:       []string{"users"}, // unqualified
	})
	if err == nil {
		t.Fatal("expected error for unqualified table name")
	}
	if !strings.Contains(err.Error(), "unqualified") {
		t.Errorf("error should mention the qualifier requirement: %v", err)
	}
}
