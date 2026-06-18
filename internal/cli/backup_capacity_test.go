package cli_test

import (
	"strings"
	"testing"
)

// TestBackup_CapacityFlagsDiscoverable: --ignore-capacity and
// --capacity-safety-factor show in `backup --help` so an
// operator hitting the gate at 3am finds the override.
func TestBackup_CapacityFlagsDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "backup", "--help")
	for _, want := range []string{
		"--ignore-capacity",
		"--capacity-safety-factor",
		"110%",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("backup --help missing %q:\n%s", want, stdout)
		}
	}
}
