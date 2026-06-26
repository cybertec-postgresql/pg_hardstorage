// demo.go — `pg_hardstorage demo`: a self-contained, throwaway
// end-to-end run (init repo → backup → restore → verify) against a
// temporary PostgreSQL spun up in Docker. It exists so a brand-new user
// can see the whole flow work in under a couple of minutes without
// configuring anything.
//
// Previously this command only printed a one-line description and
// exited 0 — it never touched Docker (issue #15). It now drives the
// real flow through the `docker` CLI (which honours DOCKER_HOST, so
// Lima / Colima / Podman-with-docker-shim setups work) and the same
// subcommands an operator would run by hand.
//
// The orchestration is written against a small commandRunner seam so
// the full step sequence, error handling, and cleanup are unit-testable
// without a Docker daemon; the real end-to-end run is exercised in CI.
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// demoImage is the PostgreSQL image the demo runs. Kept here as a
// single constant so a supported-major bump is a one-line change.
const demoImage = "postgres:18"

// commandRunner runs an external command and returns its combined
// output. The seam lets tests drive the demo without a Docker daemon
// or a second pg_hardstorage process.
type commandRunner interface {
	run(ctx context.Context, name string, args ...string) (string, error)
}

// execRunner is the production commandRunner: it shells out for real.
type execRunner struct{}

func (execRunner) run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

func newDemoCmdImpl() *cobra.Command {
	return &cobra.Command{
		Use:          "demo",
		Short:        "Run a throwaway end-to-end demo (init → backup → restore → verify) on a temporary PG in Docker",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			self, err := os.Executable()
			if err != nil {
				self = "pg_hardstorage"
			}
			if err := runDemo(cmd.Context(), cmd.ErrOrStderr(), execRunner{}, self); err != nil {
				return err
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(map[string]any{
				"status":  "ok",
				"message": "demo completed: a temporary PostgreSQL was backed up, restored, and verified, then cleaned up",
			}))
		},
	}
}

// runDemo executes the end-to-end demo. progress is written to w as the
// flow advances; r runs docker + self subcommands; self is the path to
// this binary (used to invoke the real backup/restore/verify verbs).
func runDemo(ctx context.Context, w io.Writer, r commandRunner, self string) error {
	step := func(format string, a ...any) { fmt.Fprintf(w, "  → "+format+"\n", a...) }

	// 1. Preflight: Docker must be reachable. `docker info` fails fast
	//    and clearly when the daemon (or DOCKER_HOST) isn't set up.
	fmt.Fprintln(w, "pg_hardstorage demo — spinning up a throwaway PostgreSQL in Docker")
	if out, err := r.run(ctx, "docker", "info"); err != nil {
		return output.NewError("demo.docker_unavailable",
			"demo: Docker does not appear to be reachable").
			WithSuggestion(&output.Suggestion{
				Human: "start Docker (Docker Desktop / Colima / Lima / Podman) and ensure the daemon is up. " +
					"If your socket isn't the default, set DOCKER_HOST (e.g. " +
					"export DOCKER_HOST=unix:///path/to/docker.sock). Underlying error: " + firstLine(strings.TrimSpace(out)),
			}).Wrap(err)
	}

	// 2. Start PG. POSTGRES_HOST_AUTH_METHOD=trust makes the official
	//    image emit a pg_hba `host replication all all trust` line, so
	//    BASE_BACKUP over the replication protocol works out of the box.
	//    Publishing 5432 to an ephemeral host port avoids collisions.
	step("starting %s", demoImage)
	cid, err := r.run(ctx, "docker", "run", "-d", "--rm",
		"-e", "POSTGRES_HOST_AUTH_METHOD=trust",
		"-P", demoImage,
		"-c", "wal_level=replica", "-c", "max_wal_senders=10")
	if err != nil {
		return output.NewError("demo.start_failed",
			"demo: could not start the PostgreSQL container: "+firstLine(strings.TrimSpace(cid))).Wrap(err)
	}
	cid = strings.TrimSpace(cid)
	// Always tear the container down, even on a mid-flow failure.
	defer func() {
		_, _ = r.run(context.WithoutCancel(ctx), "docker", "rm", "-f", cid)
	}()

	// 3. Resolve the published host port for 5432.
	portOut, err := r.run(ctx, "docker", "port", cid, "5432/tcp")
	if err != nil {
		return output.NewError("demo.port_failed",
			"demo: could not resolve the container's published port: "+firstLine(strings.TrimSpace(portOut))).Wrap(err)
	}
	hostPort, err := parseDockerPort(portOut)
	if err != nil {
		return output.NewError("demo.port_failed", "demo: "+err.Error()).Wrap(err)
	}

	// 4. Wait for PG to accept connections.
	step("waiting for PostgreSQL to become ready")
	if err := waitForPG(ctx, r, cid); err != nil {
		return err
	}

	// 5. Throwaway repo + restore target.
	repoDir, err := os.MkdirTemp("", "pg_hardstorage-demo-repo-")
	if err != nil {
		return output.NewError("internal", "demo: temp repo: "+err.Error()).Wrap(err)
	}
	defer func() { _ = os.RemoveAll(repoDir) }()
	restoreDir, err := os.MkdirTemp("", "pg_hardstorage-demo-restore-")
	if err != nil {
		return output.NewError("internal", "demo: temp restore dir: "+err.Error()).Wrap(err)
	}
	defer func() { _ = os.RemoveAll(restoreDir) }()

	repoURL := "file://" + repoDir
	dsn := fmt.Sprintf("postgres://postgres@127.0.0.1:%s/postgres?sslmode=disable", hostPort)

	// 6. The real flow, through the same verbs an operator runs.
	flow := []struct {
		label string
		args  []string
	}{
		{"initialising repository", []string{"repo", "init", repoURL}},
		{"taking a base backup", []string{"backup", "demo", "--pg-connection", dsn, "--repo", repoURL}},
		{"restoring the backup", []string{"restore", "demo", "latest", "--repo", repoURL, "--target", restoreDir}},
		{"verifying the backup", []string{"verify", "demo", "latest", "--repo", repoURL}},
	}
	for _, s := range flow {
		step("%s", s.label)
		if out, err := r.run(ctx, self, s.args...); err != nil {
			return output.NewError("demo.step_failed",
				fmt.Sprintf("demo: step %q failed: %s", s.label, firstLine(strings.TrimSpace(out)))).Wrap(err)
		}
	}

	fmt.Fprintln(w, "✓ demo complete — backup, restore, and verify all succeeded; cleaning up")
	return nil
}

// waitForPG polls pg_isready inside the container until PG accepts
// connections or the budget runs out.
func waitForPG(ctx context.Context, r commandRunner, cid string) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := r.run(ctx, "docker", "exec", cid, "pg_isready", "-U", "postgres"); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return output.NewError("demo.pg_not_ready",
				"demo: PostgreSQL did not become ready within 60s").Wrap(context.DeadlineExceeded)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// parseDockerPort extracts the host port from `docker port` output,
// which looks like "0.0.0.0:49153" (optionally with extra IPv6 lines).
func parseDockerPort(out string) (string, error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.LastIndex(line, ":"); i >= 0 && i < len(line)-1 {
			port := line[i+1:]
			if port != "" {
				return port, nil
			}
		}
	}
	return "", fmt.Errorf("could not parse a host port from docker output %q", strings.TrimSpace(out))
}
