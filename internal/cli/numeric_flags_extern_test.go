package cli_test

import (
	"strings"
	"testing"
)

// Salvaged from PR #30: numeric CLI options reject non-finite (NaN/Inf)
// values loudly instead of feeding a nonsensical multiplier/price into a
// gate. Go's flag parsing accepts "NaN"/"Inf" as valid float64s, so the
// guard is a real one — not caught by a plain `x <= 0` bound.
func TestNumericFlags_RejectNonFinite(t *testing.T) {
	repoURL := "file://" + t.TempDir()
	cases := []struct {
		name string
		args []string
	}{
		{"anomaly --threshold=NaN", []string{"anomaly", "check", "db1", "--repo", repoURL, "--threshold=NaN"}},
		{"anomaly --threshold=Inf", []string{"anomaly", "check", "db1", "--repo", repoURL, "--threshold=Inf"}},
		{"capacity --safety-factor=NaN", []string{"capacity", "preflight", repoURL, "--safety-factor=NaN"}},
		{"capacity --safety-factor=Inf", []string{"capacity", "preflight", repoURL, "--safety-factor=+Inf"}},
		{"cost --price-per-gb-month=NaN", []string{"cost", "report", "--repo", repoURL, "--price-per-gb-month=NaN"}},
		{"forecast --price-per-gb-month=Inf", []string{"forecast", repoURL, "--price-per-gb-month=Inf"}},
		{"insider --spike-factor=NaN", []string{"insider", "scan", "--repo", repoURL, "--spike-factor=NaN"}},
		{"repo replicate --max-mbps=Inf", []string{"repo", "replicate", "--from", repoURL, "--to", "file://" + t.TempDir(), "--max-mbps=Inf"}},
		{"repo replicate --max-mbps=NaN", []string{"repo", "replicate", "--from", repoURL, "--to", "file://" + t.TempDir(), "--max-mbps=NaN"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, stderr, exit := runCmd(t, tc.args...)
			all := out + stderr
			if exit == 0 {
				t.Fatalf("expected non-zero exit (usage error), got 0\n%s", all)
			}
			if !strings.Contains(all, "finite") {
				t.Fatalf("error should reject the value as non-finite, got:\n%s", all)
			}
		})
	}
}
