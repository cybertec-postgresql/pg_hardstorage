package pg

import (
	"strings"
	"testing"
)

// TestProbeVersionQuery_GUC is the regression guard for issue #95 (and
// its predecessor #5): the version probe must SHOW server_version, never
// the non-existent server_version_full — which made the 0.9.0 RPM fail
// every probe with `unrecognized configuration parameter
// "server_version_full"` — nor server_version_num (no vendor suffix).
func TestProbeVersionQuery_GUC(t *testing.T) {
	if probeVersionQuery != "SHOW server_version" {
		t.Fatalf("probeVersionQuery = %q, want %q", probeVersionQuery, "SHOW server_version")
	}
	for _, bad := range []string{"server_version_full", "server_version_num"} {
		if strings.Contains(probeVersionQuery, bad) {
			t.Errorf("probeVersionQuery must not reference %q (invalid/inappropriate GUC)", bad)
		}
	}
}

// TestParseVersion_PG18 covers the version the issue #95 reporter ran
// (PG 18.x): the parser must read it as major 18 so the support-window
// check and incremental-backup gating behave correctly.
func TestParseVersion_PG18(t *testing.T) {
	for _, c := range []struct {
		raw       string
		wantMajor int
		wantMinor int
	}{
		{"18.4", 18, 4},
		{"18", 18, 0},
		{"18.0 (Debian 18.0-1.pgdg120+1)", 18, 0},
	} {
		v, err := ParseVersion(c.raw)
		if err != nil {
			t.Errorf("ParseVersion(%q): %v", c.raw, err)
			continue
		}
		if v.Major != c.wantMajor || v.Minor != c.wantMinor {
			t.Errorf("ParseVersion(%q) = %d.%d, want %d.%d",
				c.raw, v.Major, v.Minor, c.wantMajor, c.wantMinor)
		}
		if v.Raw != c.raw {
			t.Errorf("ParseVersion(%q).Raw = %q, want verbatim", c.raw, v.Raw)
		}
	}
	if !IsSupportedMajor(18) {
		t.Error("PG 18 must be inside the supported-major window")
	}
}
