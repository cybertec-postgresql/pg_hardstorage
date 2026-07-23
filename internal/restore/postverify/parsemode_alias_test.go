package postverify

import (
	"strings"
	"testing"
)

func TestParseModeAliases(t *testing.T) {
	for s, want := range map[string]Mode{"skip": ModeOff, "require": ModeRequired, "off": ModeOff, "required": ModeRequired} {
		got, err := ParseMode(s)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q)=%v,%v want %v", s, got, err, want)
		}
	}
}

// Regression: the SELECT-1 probe treated psql stderr diagnostics mixed
// into CombinedOutput (e.g. the collation-version WARNING when a
// container-built cluster starts on host binaries) as failure even
// though the query answered. Only the final result row matters.
func TestProbeOutputAcceptsLeadingDiagnostics(t *testing.T) {
	ok := []string{
		"1\n",
		"WARNING:  database \"postgres\" has a collation version mismatch\nDETAIL:  x\nHINT:  y\n1\n",
	}
	for _, out := range ok {
		lines := splitProbeForTest(out)
		if len(lines) == 0 || lines[len(lines)-1] != "1" {
			t.Errorf("output %q rejected; want accepted", out)
		}
	}
	bad := []string{"", "WARNING: x\n", "2\n"}
	for _, out := range bad {
		lines := splitProbeForTest(out)
		if len(lines) > 0 && lines[len(lines)-1] == "1" {
			t.Errorf("output %q accepted; want rejected", out)
		}
	}
}

func splitProbeForTest(out string) []string {
	return strings.Fields(strings.TrimSpace(out))
}
