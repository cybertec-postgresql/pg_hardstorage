package cli_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
)

// TestAuditSummary_HelpExamplesUseRealFlags pins that every flag in
// `audit summary`'s help examples exists. The "per-tenant deletion
// volume" example used --action, which summary does not have (only
// --action-prefix), so copying it from --help failed with an unknown
// flag.
func TestAuditSummary_HelpExamplesUseRealFlags(t *testing.T) {
	cmd, _, err := cli.NewRoot().Find([]string{"audit", "summary"})
	if err != nil {
		t.Fatal(err)
	}
	flagRe := regexp.MustCompile(`--([a-z][a-z0-9-]*)`)
	for _, line := range strings.Split(cmd.Long, "\n") {
		if !strings.Contains(line, "audit summary") {
			continue
		}
		for _, m := range flagRe.FindAllStringSubmatch(line, -1) {
			if cmd.Flags().Lookup(m[1]) == nil && cmd.InheritedFlags().Lookup(m[1]) == nil {
				t.Errorf("help example uses --%s, which `audit summary` does not define: %q", m[1], strings.TrimSpace(line))
			}
		}
	}
}
