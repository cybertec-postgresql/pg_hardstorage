package assert_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/assert"
)

// These kinds were declared in the DSL and "deferred to the scenario
// runner" — which never implemented them, so each one returned
// Passed: true without checking anything. A scenario asserting
// `audit_chain_intact: true` passed against a broken chain. Until a
// runner implements them they must fail, and say why.
func TestRun_UnimplementedKindsFailLoudly(t *testing.T) {
	for _, kind := range []string{"pg_amcheck", "pg_verifybackup", "audit_chain_intact",
		"no_orphan_chunks", "no_uncommitted_manifests"} {
		r := assert.Run(t.Context(), assert.Context{}, assert.Assertion{Kind: kind, Args: true})
		if r.Passed {
			t.Errorf("%s passed without checking anything", kind)
		}
		if !strings.Contains(r.Message, "not implemented") {
			t.Errorf("%s: message %q should say it is not implemented", kind, r.Message)
		}
	}
}

func TestRun_CLIOutputContainsAny(t *testing.T) {
	ac := assert.Context{CLIOutput: "error: repo.format.future (see runbooks/upgrade)", HaveCLIOutput: true}
	ok := assert.Run(t.Context(), ac, assert.Assertion{Kind: "cli_output_contains_any",
		Args: []any{"nope", "repo.format.future"}})
	if !ok.Passed {
		t.Errorf("one substring matches; want pass: %s", ok.Message)
	}
	miss := assert.Run(t.Context(), ac, assert.Assertion{Kind: "cli_output_contains_any",
		Args: []any{"nope", "also-nope"}})
	if miss.Passed {
		t.Error("no substring matches; want fail")
	}
	none := assert.Run(t.Context(), assert.Context{}, assert.Assertion{Kind: "cli_output_contains_any",
		Args: []any{"anything"}})
	if none.Passed {
		t.Error("no cli_run ran before the assert; want fail")
	}
	empty := assert.Run(t.Context(), ac, assert.Assertion{Kind: "cli_output_contains_any", Args: []any{}})
	if empty.Passed {
		t.Error("an empty substring list checks nothing; want fail")
	}
}
