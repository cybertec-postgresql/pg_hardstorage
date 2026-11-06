package cli_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// --fetch-concurrency is validated before any repository access, on
// every command that carries it.
func TestFetchConcurrencyFlag_RangeIsEnforced(t *testing.T) {
	w := newReadWorld(t)
	for _, args := range [][]string{
		{"verify", "db1", "--repo", w.repoURL, "--fetch-concurrency", "65"},
		{"verify", "db1", "--repo", w.repoURL, "--fetch-concurrency", "-1"},
		{"verify", "db1", "--repo", w.repoURL, "--full", "--fetch-concurrency", "65"},
		{"restore", "db1", "latest", "--repo", w.repoURL, "--target", t.TempDir(), "--fetch-concurrency", "100"},
	} {
		_, stderr, exit := runCLI(t, args...)
		if exit != int(output.ExitMisuse) || !strings.Contains(stderr, "--fetch-concurrency") {
			t.Errorf("%v: exit=%d, want a usage error naming the flag:\n%s", args, exit, stderr)
		}
	}
}
