// Package pgboot boots a product-restored PostgreSQL data directory in
// a version-exact postgres:<major> container and queries it.
//
// This is the test-side answer to a blind spot four rounds of
// object-level retention tests could not close: a repository where
// every manifest verifies and every chunk exists can still hold an
// archive PostgreSQL refuses to replay. The only assertion that
// settles "can this be recovered" is a boot, and a boot must run the
// SAME major that wrote the datadir — which on a single-major host
// means a container.
//
// Environment truths this package encodes (each bought with a failed
// boot during the 2026-08 campaign):
//
//   - The image entrypoint initialises whatever $PGDATA names; without
//     it, it inspects its empty default and dies asking for
//     POSTGRES_PASSWORD, never looking at the mounted restore.
//   - A restored datadir carries the SOURCE environment's config.
//     Booting a managed-source backup (Spilo) on vanilla PostgreSQL
//     needs shared_preload_libraries cleared (absent bg_mon is FATAL),
//     ssl off (cert paths don't exist), archive_mode off (a recovered
//     clone must never run the source's archive_command), and
//     hba_file/ident_file pinned back into the datadir (managed
//     environments set them to absolute paths).
//   - postgres refuses an effective uid with no passwd entry, so the
//     boot runs as the image's postgres user (999) and the datadir is
//     chowned to 999 for the boot and back afterwards.
//   - psql sends server WARNINGs to stderr; Query captures stdout
//     ONLY, because a legitimate collation-version warning once made
//     an equality probe report a promoted server as in-recovery for
//     four straight minutes.
package pgboot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Docker runs the docker CLI and returns combined output.
func Docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Tail returns the final n bytes of s, for log excerpts.
func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// MkSharedDir creates a directory under /tmp with an explicit mode.
// Go's t.TempDir() is 0700, which a container uid cannot even
// traverse — the source of two rounds of permission failures before
// this helper existed.
func MkSharedDir(tb testing.TB, pattern string, mode os.FileMode) string {
	tb.Helper()
	d, err := os.MkdirTemp("/tmp", pattern)
	if err != nil {
		tb.Fatal(err)
	}
	if err := os.Chmod(d, mode); err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// ChownAll chowns path recursively inside a helper container, so no
// host root is needed.
func ChownAll(tb testing.TB, ctx context.Context, path, owner string) {
	tb.Helper()
	if out, err := Docker(ctx, "run", "--rm", "-v", path+":/d", "alpine:3.20",
		"chown", "-R", owner, "/d"); err != nil {
		tb.Fatalf("chown %s to %s: %v\n%s", path, owner, err, out)
	}
}

// Booted is a running recovery boot of a restored datadir.
type Booted struct {
	Name    string
	DataDir string
}

// Boot starts image over dataDir. mounts are extra -v specs (the
// product binary and repository, for restore_command); envs are extra
// -e specs (e.g. PG_HARDSTORAGE_KEYRING_DIR for encrypted archives —
// note the keystore's 0600 gate means the key must be a COPY owned by
// uid 999, not a chmod a+r of the original). Cleanup is registered on
// tb: the container is removed and the datadir chowned back to the
// calling user so TempDir cleanup works.
func Boot(tb testing.TB, ctx context.Context, image, dataDir string, mounts, envs []string) *Booted {
	tb.Helper()
	ChownAll(tb, ctx, dataDir, "999:999")

	name := fmt.Sprintf("pg-hs-boot-%d", time.Now().UnixNano())
	args := []string{"run", "-d", "--name", name, "--user", "999:999",
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
		"-c", "shared_preload_libraries=",
		"-c", "ssl=off",
		"-c", "archive_mode=off",
		"-c", "hba_file=/pgdata/pg_hba.conf",
		"-c", "ident_file=/pgdata/pg_ident.conf",
		"-c", "log_destination=stderr",
	)
	if out, err := Docker(ctx, args...); err != nil {
		tb.Fatalf("boot %s: %v\n%s", image, err, out)
	}
	b := &Booted{Name: name, DataDir: dataDir}
	tb.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = Docker(cctx, "rm", "-f", name)
		me := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
		ChownAll(tb, cctx, dataDir, me)
	})
	return b
}

// Query runs a single-value SQL statement via psql inside the boot
// container and returns trimmed STDOUT ONLY (see the package doc for
// why stdout-only is load-bearing).
func (b *Booted) Query(ctx context.Context, sql string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "exec", b.Name,
		"psql", "-h", "/tmp", "-U", "postgres", "-d", "postgres", "-tAc", sql)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stdout.String()),
			fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Logs returns the container log tail for diagnostics.
func (b *Booted) Logs(ctx context.Context) string {
	out, _ := Docker(ctx, "logs", "--tail", "80", b.Name)
	return out
}

// WaitPromoted is AwaitPromoted's non-fatal form: it returns an error
// instead of failing tb, for callers (the chaos gate) that must keep
// going after a failed sample and attach the container log themselves.
func (b *Booted) WaitPromoted(ctx context.Context, within time.Duration) error {
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		out, err := b.Query(ctx, "SELECT pg_is_in_recovery()::text")
		last = out
		if err == nil && out == "false" {
			return nil
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return fmt.Errorf("context cancelled awaiting promotion; last=%q", last)
		}
	}
	return fmt.Errorf("did not finish recovery and promote within %s (last probe: %q)", within, last)
}

// AwaitPromoted polls until the server answers queries OUTSIDE
// recovery — archive replay finished and promotion fired — or fails
// with the container log. A server that answers while still in
// recovery has not yet proven the archive replays to the end.
func (b *Booted) AwaitPromoted(tb testing.TB, ctx context.Context, within time.Duration) {
	tb.Helper()
	if err := b.WaitPromoted(ctx, within); err != nil {
		tb.Fatalf("server %v.\ncontainer log:\n%s", err, b.Logs(ctx))
	}
}
