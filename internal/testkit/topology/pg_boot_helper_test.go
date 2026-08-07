//go:build integration

package topology_test

// pg_boot_helper_test.go — thin aliases over internal/testkit/pgboot,
// which owns the container-boot machinery (and its hard-won
// environment truths — see that package's doc). The chaos soak's
// restore-proof gate uses pgboot directly; these aliases keep the
// topology tests' call sites short.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/pgboot"
)

func dockerOut(ctx context.Context, args ...string) (string, error) {
	return pgboot.Docker(ctx, args...)
}

func lastLines(s string, n int) string { return pgboot.Tail(s, n) }

func mkSharedDir(t *testing.T, pattern string, mode os.FileMode) string {
	t.Helper()
	return pgboot.MkSharedDir(t, pattern, mode)
}

func chownAll(t *testing.T, ctx context.Context, path, owner string) {
	t.Helper()
	pgboot.ChownAll(t, ctx, path, owner)
}

func bootRestoredDatadir(t *testing.T, ctx context.Context, image, dataDir string, mounts, envs []string) *pgboot.Booted {
	t.Helper()
	return pgboot.Boot(t, ctx, image, dataDir, mounts, envs)
}

// buildProductBinary returns a pg_hardstorage binary to drive.
//
// Deliberately separate from the chaos soak's own builder rather than
// shared with it: that one lives behind the `chaos` tag and carries its
// own PGHS_CHAOS_BIN override, and moving it would couple two lanes
// that currently fail independently.
func buildProductBinary(t *testing.T) string {
	t.Helper()
	root := repoRootForTopologyTests(t)
	// ALWAYS build from the working tree. Preferring a prebuilt
	// bin/pg_hardstorage is how this test first "ran": it picked up a
	// binary from the previous day, exercised code that predated the
	// fix under test, and reported a failure that had nothing to do
	// with the change. A stale artefact makes an integration test
	// answer a question nobody asked, in either direction — it can just
	// as easily go green on code that is no longer there.
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("no pg_hardstorage binary and no Go toolchain to build one: %v\n\n"+
			"This test drives the shipped CLI on purpose — the capture it verifies lives "+
			"in the stream's setup path, and calling into the package would prove the "+
			"helper works rather than that the command runs it.", err)
	}
	out := filepath.Join(t.TempDir(), "pg_hardstorage")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/pg_hardstorage")
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the binary under test failed: %v\n%s", err, b)
	}
	return out
}

// repoRootForTopologyTests walks up from this file to the module root.
func repoRootForTopologyTests(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", ".."))
}
