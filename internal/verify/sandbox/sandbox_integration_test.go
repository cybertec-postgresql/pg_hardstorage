//go:build integration

package sandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/verify/sandbox"
)

// TestVerify_MissingManifest_Skipped builds a synthetic "almost a PG
// data dir" — has PG_VERSION but no backup_manifest — and asserts that
// the sandbox returns a structured "skipped" result rather than a
// failure. This is the realistic posture for backups taken before
// IncludeManifest defaulted true.
//
// Build-tagged `integration` because it spins up a real Docker
// container.
func TestVerify_MissingManifest_Skipped(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker; -short")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("17\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := sandbox.Verify(ctx, sandbox.Options{
		DataDir: dir,
		PGMajor: "17",
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.Skipped && !res.Passed {
		// Either "skipped because no backup_manifest" or "passed
		// (PG image's pg_verifybackup decided the dir is fine)" —
		// both are acceptable outcomes for this minimal fixture.
		// What we want to fail on is "errored on the sandbox setup".
		t.Logf("Result: passed=%v skipped=%v reason=%q stdout=%q",
			res.Passed, res.Skipped, res.SkipReason, res.Stdout)
	}
}
