package cli_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"

	// Same side-effect imports the production binary triggers
	// transitively via internal/cli.  These test imports are
	// belt-and-suspenders: if a future refactor accidentally
	// drops the side-effect import from output_flags.go, the
	// test still passes — but the production binary would
	// silently lose the sink.  TestProductionRegistration_*
	// below catches that case by importing only the cli
	// package (no _ aliases) and asserting every claimed sink
	// is in the registry.
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
)

// TestProductionRegistration_SinksFire walks the v0.9+ Tier-1
// sink list and asserts every plugin name is present in
// output.DefaultSinkRegistry — i.e., its init() actually fired
// when `internal/cli` was loaded.
//
// This guards against the audit-v26 class of bug where a new
// plugin compiles but never reaches the running binary because
// nobody imported it for side-effects.  The original case
// surfaced for awskms/gcpkms (init never fired); this test
// closes the same gap for sinks.
func TestProductionRegistration_SinksFire(t *testing.T) {
	want := []string{
		"cef",
		"datadog-events",
		"discord",
		"email",
		"jira",
		"opsgenie",
		"otel-events",
		"pagerduty",
		"servicenow",
		"slack",
		"splunk-hec",
		"syslog",
		"teams",
		"webhook",
	}
	sort.Strings(want)

	got := output.DefaultSinkRegistry.Plugins()
	gotSet := map[string]bool{}
	for _, n := range got {
		gotSet[n] = true
	}

	var missing []string
	for _, w := range want {
		if !gotSet[w] {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		t.Errorf("sink plugins not registered (init() never fired in production binary): %s\nfull registered list: %s",
			strings.Join(missing, ", "),
			strings.Join(got, ", "))
	}
}
