package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/tools"
)

// TestMislabel_TriggersRetry is the round-2 #2 regression: a destructive
// command the model labeled a "dry-run" now feeds the same retry loop as a
// structural error, so — given a budget — the model gets re-prompted and the
// corrected reply wins, instead of the mislabel only being warned about after
// the fact.
func TestMislabel_TriggersRetry(t *testing.T) {
	clean := "Preview first (no --apply):\n```bash\npg_hardstorage rotate db1 --repo r\n```\n\nThen to execute:\n```bash\npg_hardstorage rotate db1 --repo r --apply\n```"
	s := &Session{
		Provider:            &scriptedProvider{turns: []scriptedTurn{{text: clean}}},
		Tools:               tools.NewRegistry(),
		MaxValidatorRetries: 1,
		MaxToolCallsPerTurn: 2,
		// no CommandValidator → only the intent-vs-effect scan runs
	}
	first := &Reply{Text: "```bash\n# Dry-run first — nothing is deleted\npg_hardstorage rotate db1 --apply --repo r\n```"}

	out := s.validateAndMaybeRetry(context.Background(), first)

	if len(out.CommandWarnings) != 0 {
		t.Errorf("retry should have cleared the mislabel; got: %+v", out.CommandWarnings)
	}
	if out.Text != clean {
		t.Errorf("the corrected reply should win; got: %q", out.Text)
	}
}

// TestMislabel_SurfacesWithoutRetryBudget: with no retry budget the mislabel
// still surfaces as a warning — the operator is never left thinking a delete
// is a safe dry-run. (No provider needed: the budget is exhausted at attempt 0.)
func TestMislabel_SurfacesWithoutRetryBudget(t *testing.T) {
	s := &Session{MaxValidatorRetries: 0}
	reply := &Reply{Text: "```bash\n# safe dry-run, won't change anything\npg_hardstorage repo gc --apply --repo r\n```"}

	out := s.validateAndMaybeRetry(context.Background(), reply)

	if len(out.CommandWarnings) != 1 {
		t.Fatalf("expected 1 mislabel warning, got %d: %+v", len(out.CommandWarnings), out.CommandWarnings)
	}
	if !strings.Contains(out.CommandWarnings[0].Issue, "DESTRUCTIVE") {
		t.Errorf("warning should name it destructive: %+v", out.CommandWarnings[0])
	}
}
