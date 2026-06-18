package replication

import "testing"

// Pure-unit tests for the preflight building blocks.  The
// PG-touching paths (the actual pg_settings reads) are covered by
// preflight_integration_test.go under -tags=integration.

func TestWalLevelOK(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"replica", true},
		{"REPLICA", true},
		{" replica ", true},
		{"logical", true},
		{"minimal", false},
		{"archive", false}, // legacy PG <9.6 setting; rejected
		{"", false},
	}
	for _, c := range cases {
		got := walLevelOK(c.in)
		if got != c.want {
			t.Errorf("walLevelOK(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPreflightResult_HasFatal(t *testing.T) {
	cases := []struct {
		name string
		in   PreflightResult
		want bool
	}{
		{"empty", PreflightResult{}, false},
		{"only-warnings", PreflightResult{Findings: []PreflightFinding{
			{Severity: PreflightWarning, Code: "x"},
			{Severity: PreflightInfo, Code: "y"},
		}}, false},
		{"one-fatal", PreflightResult{Findings: []PreflightFinding{
			{Severity: PreflightWarning, Code: "x"},
			{Severity: PreflightFatal, Code: "y"},
		}}, true},
	}
	for _, c := range cases {
		got := c.in.HasFatal()
		if got != c.want {
			t.Errorf("%s: HasFatal = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSyncStandbyNamesContains(t *testing.T) {
	const me = "pg_hardstorage_db1"
	cases := []struct {
		guc  string
		want bool
	}{
		{"", false},
		{"  ", false},
		{"*", true},
		{"pg_hardstorage_db1", true},
		{"other", false},
		{"a, pg_hardstorage_db1, b", true},
		{"a, b, c", false},
		{`"pg_hardstorage_db1"`, true},
		{"FIRST 2 (a, pg_hardstorage_db1, b)", true},
		{"FIRST 1 (a, b)", false},
		{"ANY 2 (x, pg_hardstorage_db1)", true},
		{"any 1 (*)", true},
	}
	for _, c := range cases {
		if got := syncStandbyNamesContains(c.guc, me); got != c.want {
			t.Errorf("syncStandbyNamesContains(%q, %q) = %v, want %v", c.guc, me, got, c.want)
		}
	}
	if syncStandbyNamesContains("*", "") {
		t.Error("empty name must never match")
	}
}
