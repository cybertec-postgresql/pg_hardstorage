package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func TestRareFloatFlagsRejectNonFiniteValues(t *testing.T) {
	commands := [][]string{
		{"backup", "db1", "--pg-connection", "postgres://unused", "--repo", "file:///missing", "--capacity-safety-factor"},
		{"capacity", "preflight", "file:///missing", "--projected-bytes", "1", "--safety-factor"},
		{"repo", "replicate", "--from", "file:///from", "--to", "file:///to", "--max-mbps"},
		{"anomaly", "check", "db1", "--repo", "file:///missing", "--threshold"},
		{"insider", "scan", "--repo", "file:///missing", "--spike-factor"},
		{"forecast", "file:///missing", "--price-per-gb-month"},
		{"cost", "report", "--repo", "file:///missing", "--price-per-gb-month"},
	}
	for _, base := range commands {
		for _, value := range []string{"NaN", "+Inf", "-Inf"} {
			name := strings.Join(base[:min(2, len(base))], "_") + "_" + value
			t.Run(name, func(t *testing.T) {
				args := append(append([]string(nil), base...), value)
				stdout, stderr, exit := runCmd(t, args...)
				if exit != int(output.ExitMisuse) {
					t.Fatalf("exit=%d, want %d\nstdout=%s\nstderr=%s", exit, output.ExitMisuse, stdout, stderr)
				}
				if !strings.Contains(stdout+stderr, "finite") {
					t.Fatalf("expected finite-value error\nstdout=%s\nstderr=%s", stdout, stderr)
				}
			})
		}
	}
}

func TestRepairScrubRejectsNegativeLimitBeforeRepoAccess(t *testing.T) {
	stdout, stderr, exit := runCmd(t,
		"repair", "scrub", "--repo", "file:///missing", "--limit", "-1")
	if exit != int(output.ExitMisuse) {
		t.Fatalf("exit=%d, want %d\nstdout=%s\nstderr=%s", exit, output.ExitMisuse, stdout, stderr)
	}
	if !strings.Contains(stdout+stderr, "--limit must be >= 0") {
		t.Fatalf("unexpected error\nstdout=%s\nstderr=%s", stdout, stderr)
	}
}

func TestMutatingRareCommandsRespectReadOnlyMode(t *testing.T) {
	tests := []struct {
		name string
		args func(t *testing.T, repoURL string) []string
	}{
		{"rotate_apply", func(_ *testing.T, u string) []string {
			return []string{"rotate", "--repo", u, "--apply"}
		}},
		{"repair_chunks_apply", func(_ *testing.T, u string) []string {
			return []string{"repair", "chunks", "--orphans", "--apply", "--repo", u}
		}},
		{"repair_scrub_heal", func(_ *testing.T, u string) []string {
			return []string{"repair", "scrub", "--heal", "--replica", "file:///different", "--repo", u}
		}},
		{"integrity_run", func(_ *testing.T, u string) []string {
			return []string{"integrity", "run", "--repo", u}
		}},
		{"insider_scan", func(_ *testing.T, u string) []string {
			return []string{"insider", "scan", "--repo", u}
		}},
		{"bundle_import", func(t *testing.T, u string) []string {
			in := filepath.Join(t.TempDir(), "bundle.tar")
			if err := os.WriteFile(in, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return []string{"repo", "bundle", "import", "--to", u, "--in", in}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repoURL := initRepoForTest(t)
			if _, err := repo.SetMode(context.Background(), repo.SetModeOptions{
				URL: repoURL, Mode: repo.ModeReadOnly,
			}); err != nil {
				t.Fatalf("set read-only: %v", err)
			}
			stdout, stderr, exit := runCmd(t, tc.args(t, repoURL)...)
			if exit != int(output.ExitConflict) {
				t.Fatalf("exit=%d, want %d\nstdout=%s\nstderr=%s", exit, output.ExitConflict, stdout, stderr)
			}
			if !strings.Contains(stdout+stderr, "repo_read_only") {
				t.Fatalf("missing read-only error\nstdout=%s\nstderr=%s", stdout, stderr)
			}
		})
	}
}
