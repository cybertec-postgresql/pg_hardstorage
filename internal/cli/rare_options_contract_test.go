package cli

import (
	"strings"
	"testing"
)

func TestRepairScrubDoesNotAdvertisePhantomDryRunFlag(t *testing.T) {
	cmd := newRepairScrubCmd()
	if cmd.Flags().Lookup("dry-run-heal") != nil {
		t.Fatal("unexpected --dry-run-heal flag")
	}
	if strings.Contains(cmd.Long, "--dry-run-heal") {
		t.Fatal("help advertises nonexistent --dry-run-heal")
	}
}

func TestServerHelpDescribesFlagOnlyRuntimeConfiguration(t *testing.T) {
	long := newRealServerCmd().Long
	if strings.Contains(long, "flags below override the config file's server") {
		t.Fatal("server help still promises an unsupported server: config block")
	}
	if !strings.Contains(long, "flag-only") {
		t.Fatal("server help does not explain the current flag-only contract")
	}
}
