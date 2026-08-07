//go:build integration

package topology_test

// pg_boot_helper_test.go — boot a product-restored datadir in a
// version-exact postgres:<major> container and query it.
//
// Why containers rather than host binaries: the host carries one PG
// major (16), boot-verifying a restored datadir needs the SAME major
// that wrote it, and installing four majors system-wide needs root
// this environment does not have. A container is version-exact, needs
// no root, and extends to a new major the day its image exists.
//
// Why chown to uid 999: postgres refuses to run as an effective user
// with no passwd entry, and an arbitrary host uid has none inside the
// image. The image's postgres user (999) does. The helper chowns the
// datadir to 999 for the boot and back to the caller's uid afterwards
// so t.TempDir cleanup can do its job.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// mkSharedDir creates a directory under /tmp with an explicit mode.
// Go's t.TempDir() is 0700, which a container uid (999) cannot even
// traverse — the source of two rounds of permission failures before
// this helper existed.
func mkSharedDir(t *testing.T, pattern string, mode os.FileMode) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", pattern)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// dockerOut runs docker with args and returns combined output.
func dockerOut(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// chownAll chowns path recursively inside a helper container (no host
// root needed).
func chownAll(t *testing.T, ctx context.Context, path, owner string) {
	t.Helper()
	if out, err := dockerOut(ctx, "run", "--rm", "-v", path+":/d", "alpine:3.20",
		"chown", "-R", owner, "/d"); err != nil {
		t.Fatalf("chown %s to %s: %v\n%s", path, owner, err, out)
	}
}

// bootedPG is a running recovery boot of a restored datadir.
type bootedPG struct {
	name    string
	dataDir string
}

// bootRestoredDatadir starts postgres:<major> over dataDir. mounts are
// extra -v specs (binary + repo for restore_command). The container
// runs the postmaster directly — no image entrypoint, no initdb — and
// exposes only a unix socket inside the container.
func bootRestoredDatadir(t *testing.T, ctx context.Context, image, dataDir string, mounts, envs []string) *bootedPG {
	t.Helper()
	chownAll(t, ctx, dataDir, "999:999")

	name := fmt.Sprintf("pg-hs-boot-%d", time.Now().UnixNano())
	args := []string{"run", "-d", "--name", name, "--user", "999:999",
		// The image ENTRYPOINT initialises whatever $PGDATA points at.
		// Without this it inspects its default (/var/lib/postgresql/
		// data), finds it empty, and dies asking for POSTGRES_PASSWORD
		// — never looking at the restored tree we mounted.
		"-e", "PGDATA=/pgdata",
		"-v", dataDir + ":/pgdata"}
	for _, m := range mounts {
		args = append(args, "-v", m)
	}
	for _, e := range envs {
		args = append(args, "-e", e)
	}
	args = append(args, image,
		"postgres", "-D", "/pgdata",
		"-c", "unix_socket_directories=/tmp",
		"-c", "listen_addresses=",
		"-c", "logging_collector=off",
		// The restored datadir faithfully carries the SOURCE
		// environment's config, and a vanilla postgres image does not
		// ship that environment. Command-line -c wins over every file
		// source, which makes these the honest DR overrides — the same
		// ones an operator restoring, say, a Spilo backup onto plain
		// PostgreSQL must apply:
		//   - shared_preload_libraries: Spilo preloads bg_mon et al.;
		//     absent .so files are FATAL at boot ("could not access
		//     file \"bg_mon\"").
		//   - ssl: the source's cert paths do not exist here.
		//   - archive_mode: the source's archive_command must not run
		//     from the booted copy — its destinations do not exist
		//     here, and a recovered clone re-archiving into the
		//     production repo would be worse than a failure.
		"-c", "shared_preload_libraries=",
		"-c", "ssl=off",
		"-c", "archive_mode=off",
		//   - hba_file/ident_file: managed environments pin these to
		//     ABSOLUTE paths (Spilo: /home/postgres/pgdata/...), which
		//     do not exist in the boot container. The files themselves
		//     ride inside the backup, so point back into the datadir.
		"-c", "hba_file=/pgdata/pg_hba.conf",
		"-c", "ident_file=/pgdata/pg_ident.conf",
		//   - log_destination: Spilo sets csvlog, which needs the
		//     collector this helper disables.
		"-c", "log_destination=stderr",
	)
	if out, err := dockerOut(ctx, args...); err != nil {
		t.Fatalf("boot %s: %v\n%s", image, err, out)
	}
	b := &bootedPG{name: name, dataDir: dataDir}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = dockerOut(cctx, "rm", "-f", name)
		// Give the tree back to the test user so TempDir cleanup works.
		me := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
		chownAll(t, cctx, dataDir, me)
	})
	return b
}

// query runs a single-value SQL statement via psql inside the boot
// container and returns trimmed STDOUT ONLY.
//
// Stdout-only is load-bearing, not tidiness: server-side WARNINGs go
// to psql's stderr, and a datadir restored from a different libc
// (Spilo's glibc 2.35 booted on 2.41 here) legitimately warns about a
// collation version mismatch on every connection. With combined
// output, awaitPromoted's `== "false"` probe compared against
// "WARNING: …\nfalse" and reported a fully promoted, query-serving
// server as still-in-recovery for four straight minutes.
func (b *bootedPG) query(ctx context.Context, sql string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "exec", b.name,
		"psql", "-h", "/tmp", "-U", "postgres", "-d", "postgres", "-tAc", sql)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		return strings.TrimSpace(stdout.String()),
			fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// logs returns the container log tail for diagnostics.
func (b *bootedPG) logs(ctx context.Context) string {
	out, _ := dockerOut(ctx, "logs", "--tail", "80", b.name)
	return out
}

// awaitPromoted polls until the server answers queries OUTSIDE
// recovery (i.e. archive replay finished and --to-action promote
// fired), or fails with the container log. This is the moment the
// whole proof exists for: a server that answers while still in
// recovery has not yet proven the archive replays to the end.
func (b *bootedPG) awaitPromoted(t *testing.T, ctx context.Context, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		out, err := b.query(ctx, "SELECT pg_is_in_recovery()::text")
		last = out
		if err == nil && out == "false" {
			return
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			t.Fatalf("context cancelled awaiting promotion; last=%q", last)
		}
	}
	t.Fatalf("server did not finish recovery and promote within %s (last probe: %q).\n"+
		"container log:\n%s", within, last, b.logs(ctx))
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

// lastLines returns the final n bytes of s, for log excerpts. The
// internal package has an equivalent; this file is in topology_test and
// cannot see it.
func lastLines(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
