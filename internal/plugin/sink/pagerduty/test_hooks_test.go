package pagerduty

import "github.com/cybertec-postgresql/pg_hardstorage/internal/output"

// MapSeverityForTest exposes the package-private mapSeverity. Tests
// in this directory exercise the mapping without poking at private
// symbols at test time.
func MapSeverityForTest(s output.Severity) string { return mapSeverity(s) }

// DedupKeyForTest exposes dedupKeyFor. Same rationale.
func DedupKeyForTest(ev *output.Event) string { return dedupKeyFor(ev) }

// OverrideEventsAPIv2URL replaces the URL the production sink uses
// so tests can point at an httptest.Server. Returns a restorer the
// test defers. The override is implemented as a package-level var
// (declared in pagerduty.go) that defaults to EventsAPIv2URL.
func OverrideEventsAPIv2URL(u string) func() {
	prev := apiURL
	apiURL = u
	return func() { apiURL = prev }
}
