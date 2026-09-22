package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// `partial dump --tables public.does_not_exist` is an operator naming
// a table that is not in the database they connected to — issue #97,
// where the table usually lives in another database and `--database`
// is the answer.
//
// Two things were wrong. The refusal routed to exit 1, because
// `partial.*` has no namespace route and no leaf route existed, so a
// not-found was indistinguishable from a crash by exit code. And it
// was raised from two places that could not both be reached: an
// empty-dump guard written for pg_dump builds that exit 0 emitting
// nothing, and — on every current pg_dump, which errors instead — the
// Dump error path, which did not raise it at all.

// TestNoTablesMatchedIsNotFound pins the exit code. An operator's
// `|| exit` wrapper, and any automation that distinguishes "fix your
// arguments" from "the tool broke", reads this number.
func TestNoTablesMatchedIsNotFound(t *testing.T) {
	err := noTablesMatchedError(partialDumpFlags{deployment: "db1"}, []string{"public.absent"})
	if got := output.ExitCodeFor(err); got != output.ExitNotFound {
		t.Errorf("a table that does not exist must exit %d (ExitNotFound), got %d — "+
			"exit 1 makes it indistinguishable from pg_dump crashing",
			output.ExitNotFound, got)
	}
}

// TestNoTablesMatchedCarriesActionableAdvice: the whole point of
// separating this from a dump failure is that there is something the
// operator can do. Losing the suggestion makes the distinction
// pointless.
func TestNoTablesMatchedCarriesActionableAdvice(t *testing.T) {
	err := noTablesMatchedError(partialDumpFlags{deployment: "db1"}, []string{"public.absent"})

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatal("must be a structured *output.Error so automation can key on the code")
	}
	if oe.Code != "partial.dump_no_tables" {
		t.Errorf("code = %q, want partial.dump_no_tables", oe.Code)
	}
	if !strings.Contains(err.Error(), "public.absent") {
		t.Error("the message must name the table the operator asked for")
	}
	if oe.Suggestion == nil || !strings.Contains(oe.Suggestion.Human, "--database") {
		t.Error("the suggestion must point at --database; that is the actual fix for #97")
	}
}

// TestNoTablesMatchedNamesTheDatabaseItSearched closes the loop on the
// advice: "not found in postgres" is what tells the operator they were
// looking in the wrong database. Defaulting matters because --database
// is optional and empty means postgres.
func TestNoTablesMatchedNamesTheDatabaseItSearched(t *testing.T) {
	for _, tc := range []struct{ flag, want string }{
		{"", "postgres"},
		{"app", "app"},
	} {
		err := noTablesMatchedError(
			partialDumpFlags{deployment: "db1", database: tc.flag},
			[]string{"public.absent"})
		if !strings.Contains(err.Error(), `"`+tc.want+`"`) {
			t.Errorf("--database=%q: message must name database %q, got: %s",
				tc.flag, tc.want, err.Error())
		}
	}
}
