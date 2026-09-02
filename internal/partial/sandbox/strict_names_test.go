package sandbox

// strict_names_test.go — a partial dump silently omitted tables that
// were not there.
//
// buildPGDumpArgs emits one --table=<name> per requested table.
// pg_dump errors with "no matching tables were found" only when NO
// pattern matches anything at all; when SOME match it dumps those,
// exits 0, and says nothing about the rest.
//
// So `partial dump --tables public.a,public.b,public.c` where only
// public.a exists produced a dump of public.a, exit 0, and an operator
// holding a file they believe contains three tables. The realistic
// causes are ordinary: a typo, or a table created after the backup was
// taken and therefore absent from the sandbox.
//
// The all-missing case was already guarded — runPartialDump's
// zero-byte check, added for issue #97, catches "pg_dump produced no
// output". That guard is what makes the partial-match case easy to
// miss: the obvious half was covered, so the gap looked closed.
//
// --strict-names (PG 9.6+; this tool targets 14+) makes pg_dump fail
// when ANY pattern matches nothing, which is the general form of the
// same check.

import (
	"strings"
	"testing"
)

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestBuildPGDumpArgs_UsesStrictNames(t *testing.T) {
	args := buildPGDumpArgs("/sock", "alice", "mydb",
		[]string{"public.a", "public.b", "public.c"}, false)

	if !hasArg(args, "--strict-names") {
		t.Fatalf("--strict-names is absent from %v.\n\nWithout it pg_dump exits 0 after "+
			"dumping only the tables that matched, so a request for three tables where one "+
			"is missing yields a dump of two and a success report. The zero-byte guard in "+
			"runPartialDump only catches the case where NOTHING matched.", args)
	}
}

// The flag must precede the patterns it governs, and every requested
// table must still be asked for.
func TestBuildPGDumpArgs_StrictNamesPrecedesTablePatterns(t *testing.T) {
	tables := []string{"public.a", "s.b"}
	args := buildPGDumpArgs("/sock", "alice", "mydb", tables, true)

	strictAt, firstTableAt := -1, -1
	for i, a := range args {
		if a == "--strict-names" && strictAt < 0 {
			strictAt = i
		}
		if strings.HasPrefix(a, "--table=") && firstTableAt < 0 {
			firstTableAt = i
		}
	}
	if strictAt < 0 || firstTableAt < 0 {
		t.Fatalf("missing --strict-names or --table in %v", args)
	}
	if strictAt > firstTableAt {
		t.Errorf("--strict-names at %d comes after the first --table at %d", strictAt, firstTableAt)
	}
	for _, tbl := range tables {
		if !hasArg(args, "--table="+tbl) {
			t.Errorf("requested table %q was dropped from %v", tbl, args)
		}
	}
	if !hasArg(args, "--data-only") {
		t.Errorf("--data-only was dropped from %v", args)
	}
}

// A single-table request gets the same treatment — the flag is not
// conditional on how many tables were asked for.
func TestBuildPGDumpArgs_StrictNamesForASingleTable(t *testing.T) {
	args := buildPGDumpArgs("/sock", "alice", "", []string{"public.only"}, false)
	if !hasArg(args, "--strict-names") {
		t.Errorf("--strict-names absent for a single-table request: %v", args)
	}
}
