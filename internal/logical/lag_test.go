package logical_test

import (
	"context"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical"
)

// TestLag_RequiresConnAndSlotName surfaces the validation guards.
// The full flow is exercised in the integration test (build-tagged
// `integration`) — these unit tests just lock down the fast-fail
// paths so a misconfigured caller doesn't silently degrade.
func TestLag_RequiresConnAndSlotName(t *testing.T) {
	_, err := logical.Lag(context.Background(), "", "slot1")
	if err == nil {
		t.Error("expected error for empty pg connection string")
	}

	_, err = logical.Lag(context.Background(), "postgres://localhost/x", "")
	if err == nil {
		t.Error("expected error for empty slot name")
	}
}
