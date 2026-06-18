package cli_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestRepoCompact_AcceptsRepoFlagAndPositional pins the surface-consistency
// fix: `repo compact` must take the repo URL the same way as its siblings
// (repo gc/audit) — via --repo OR the <url> positional — instead of
// rejecting --repo as an unknown flag. The compaction engine is still
// deferred, so the command returns the notimpl scaffold error; the point is
// that it gets there WITHOUT a flag-parse failure.
func TestRepoCompact_AcceptsRepoFlagAndPositional(t *testing.T) {
	for _, args := range [][]string{
		{"repo", "compact", "--repo", "s3://bucket/prefix"},
		{"repo", "compact", "s3://bucket/prefix"},
		{"repo", "compact", "--repo", "s3://bucket/prefix", "--apply"},
	} {
		stdout, stderr, exit := runCLI(t, args...)
		out := stdout + stderr
		if strings.Contains(out, "unknown flag") {
			t.Errorf("%v: --repo/--apply must be accepted, got unknown-flag error:\n%s", args, out)
		}
		// Deferred engine → notimpl scaffold error (exit 1), NOT a misuse
		// exit-2 from flag parsing.
		if exit != int(output.ExitError) {
			t.Errorf("%v: exit = %d, want ExitError (notimpl); out:\n%s", args, exit, out)
		}
		if !strings.Contains(out, "notimpl.compact") {
			t.Errorf("%v: expected notimpl.compact scaffold error, got:\n%s", args, out)
		}
	}
}

// TestRepoCompact_RepoPositionalConflict mirrors repo gc: a positional URL
// that disagrees with --repo is a usage error, not silently accepted.
func TestRepoCompact_RepoPositionalConflict(t *testing.T) {
	stdout, stderr, exit := runCLI(t, "repo", "compact", "s3://a", "--repo", "s3://b")
	out := stdout + stderr
	if exit != int(output.ExitMisuse) {
		t.Errorf("conflict should be ExitMisuse, got exit=%d:\n%s", exit, out)
	}
	if !strings.Contains(out, "usage.repo_conflict") {
		t.Errorf("expected usage.repo_conflict, got:\n%s", out)
	}
}
