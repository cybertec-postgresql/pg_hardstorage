package redact_test

// Redaction's failure mode is not an error — it is SILENT
// NON-REDACTION. strategyToSQLExpr had no way to signal "I do not know
// this strategy", so its fallback emitted the bare column expression:
//
//	"email" = "email"
//
// The UPDATE runs, the command reports success, and the PII is still
// there. For a tool whose entire job is keeping production data out of
// a non-production environment, that is the one outcome that has to be
// impossible rather than merely unlikely.
//
// ParseRules validated strategies, so the fallback was unreachable
// through the CLI. But NewPlan — the constructor — took a *Rules a
// caller can build in Go and validated nothing, so the guarantee rested
// on every future caller remembering to parse first. Same shape as the
// CAS's HasChunk hole: shut by accident rather than by construction.

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/redact"
)

// The regression: a Rules built in Go, bypassing ParseRules.
func TestNewPlan_RejectsAnUnknownStrategy(t *testing.T) {
	rules := &redact.Rules{
		Tables: map[string]redact.TableRules{
			"public.users": {Columns: map[string]redact.Strategy{
				"email": "obfuscate", // not a strategy this tool has
			}},
		},
	}
	_, err := redact.NewPlan(rules)
	if err == nil {
		t.Fatal("NewPlan accepted an unknown strategy — the SQL builder has no way to report " +
			"one, so the generated UPDATE would set the column to itself, report success, and " +
			"leave the data unredacted")
	}
	if !strings.Contains(err.Error(), "unknown strategy") {
		t.Errorf("error should name the cause: %v", err)
	}
}

// The malformed-regex half: a strategy that looks right and is missing
// its replacement.
func TestNewPlan_RejectsAMalformedRegexStrategy(t *testing.T) {
	rules := &redact.Rules{
		Tables: map[string]redact.TableRules{
			"public.users": {Columns: map[string]redact.Strategy{
				"note": "regex:[0-9]+", // no ":<replacement>"
			}},
		},
	}
	if _, err := redact.NewPlan(rules); err == nil {
		t.Fatal("NewPlan accepted regex strategy with no replacement half — the column would " +
			"have been emitted unchanged")
	}
}

func TestNewPlan_NilRulesRejected(t *testing.T) {
	if _, err := redact.NewPlan(nil); err == nil {
		t.Error("NewPlan(nil) must error rather than panic later")
	}
}

// Every valid strategy must still produce SQL that actually CHANGES the
// column — a test that only checked "no error" would pass against the
// fallback that emits the column unchanged.
func TestPlanSQL_EveryStrategyRewritesTheColumn(t *testing.T) {
	strategies := []redact.Strategy{
		"nullify", "hash_to_uuid", "hash_keep_domain", "replace_with_xxx",
		"constant:REDACTED", `regex:[0-9]:X`,
	}
	for _, strat := range strategies {
		t.Run(string(strat), func(t *testing.T) {
			plan, err := redact.NewPlan(&redact.Rules{
				Tables: map[string]redact.TableRules{
					"public.users": {Columns: map[string]redact.Strategy{"email": strat}},
				},
			})
			if err != nil {
				t.Fatalf("NewPlan(%q): %v", strat, err)
			}
			stmts := plan.SQL()
			if len(stmts) != 1 {
				t.Fatalf("got %d statements, want 1", len(stmts))
			}
			sql := stmts[0].Stmt
			// The identity assignment is exactly what the old fallback
			// produced, and it is a no-op that reports success.
			if strings.Contains(sql, `"email" = "email"`) {
				t.Errorf("strategy %q generated an identity assignment — the UPDATE would "+
					"leave the column unredacted:\n%s", strat, sql)
			}
			if !strings.Contains(sql, `"email" =`) {
				t.Errorf("strategy %q did not assign the column at all:\n%s", strat, sql)
			}
		})
	}
}

// ParseRules must keep rejecting the same things, so the two validation
// points cannot drift apart.
func TestParseRules_StillRejectsWhatNewPlanRejects(t *testing.T) {
	for _, body := range []string{
		"tables:\n  public.users:\n    columns:\n      email: obfuscate\n",
		"tables:\n  public.users:\n    columns:\n      note: \"regex:[0-9]+\"\n",
	} {
		if _, err := redact.ParseRules([]byte(body)); err == nil {
			t.Errorf("ParseRules accepted what NewPlan rejects:\n%s", body)
		}
	}
}
