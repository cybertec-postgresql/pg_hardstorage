// Package testkit spins up ephemeral PostgreSQL servers for tests.
//
// All real-PG tests in this repo go through this helper rather than
// each calling testcontainers-go directly, so:
//
//   - Container startup is consistent (same logging, same wait
//     strategy, same defaults).
//   - The "Docker is not available" path is exactly one branch:
//     SkipIfNoDocker on a *testing.T, called once at the top of every
//     integration test.
//
// The whole package is build-tagged `integration` so the default
// `go test ./...` run never depends on Docker. Run integration tests
// with `make test-integration` or `go test -tags=integration ./...`.
//
//go:build integration

package testkit

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// dockerHosts lists the candidate Docker daemon endpoints we probe in
// SkipIfNoDocker. Order matches typical priority on macOS and Linux.
var dockerHosts = []string{
	"unix:///var/run/docker.sock",
	"unix:///Users/" + os.Getenv("USER") + "/.docker/run/docker.sock",
	"unix:///run/user/1000/docker.sock",
}

// SkipIfNoDocker calls t.Skip() with a clear message when no reachable
// Docker daemon is found. Call this at the top of every integration
// test so a developer without Docker still sees a green build.
//
// We probe the candidate sockets directly (instead of asking
// testcontainers-go to fail-and-recover) because testcontainers' own
// "no daemon" error path takes seconds and emits noise.
func SkipIfNoDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("DOCKER_HOST") != "" {
		// Trust the user's explicit override; let testcontainers handle it.
		return
	}
	for _, h := range dockerHosts {
		if !strings.HasPrefix(h, "unix://") {
			continue
		}
		path := strings.TrimPrefix(h, "unix://")
		conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
	}
	t.Skip("Docker daemon not reachable; skipping integration test (run `make test-integration` with Docker running)")
}

// Postgres is a running PostgreSQL container plus its connection string.
type Postgres struct {
	Container testcontainers.Container
	DSN       string // libpq URI: postgres://user:pass@host:port/db?sslmode=disable
}

// DataDir returns the running container's PostgreSQL data directory
// (PGDATA), resolved from the image at runtime rather than hardcoded.
//
// This MUST be queried, not assumed: the official postgres:18 image
// relocated PGDATA from /var/lib/postgresql/data (used by 15/16/17) to
// /var/lib/postgresql/18/docker. Any test that writes into PGDATA —
// rewriting pg_hba.conf, deleting postgresql.auto.conf — silently
// targets a nonexistent path (or no-ops) on PG 18 if it hardcodes the
// old location. We read $PGDATA, which the official image sets via ENV
// for whichever major it ships.
func (p *Postgres) DataDir(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Multiplexed() demuxes the docker-exec frame headers so we get a
	// clean path rather than the 8-byte-framed stream.
	rc, reader, err := p.Container.Exec(ctx,
		[]string{"sh", "-c", `printf %s "$PGDATA"`},
		tcexec.Multiplexed(),
	)
	if err != nil {
		t.Fatalf("testkit: resolve PGDATA: %v", err)
	}
	var out string
	if reader != nil {
		b, _ := io.ReadAll(reader)
		out = strings.TrimSpace(string(b))
	}
	if rc != 0 || out == "" {
		t.Fatalf("testkit: resolve PGDATA: rc=%d out=%q (is $PGDATA set in the image?)", rc, out)
	}
	return out
}

// ExpectedPGMajor returns the PG major (as a string, e.g. "17") that
// StartPostgres will launch, honouring PG_HARDSTORAGE_TEST_PG_MAJOR
// (set per-cell by the CI integration matrix) and defaulting to 17. An
// unrecognised value falls back to 17 so a stray env value can't break
// every integration test with a bad image tag.
//
// Tests that assert on a restored cluster's PG_VERSION MUST compare
// against this rather than a hardcoded major, otherwise they only pass
// on whichever single major the matrix happens to run (historically 17,
// when the matrix was a silent no-op).
func ExpectedPGMajor() string {
	switch m := strings.TrimSpace(os.Getenv("PG_HARDSTORAGE_TEST_PG_MAJOR")); m {
	case "15", "16", "18":
		return m
	default:
		return "17"
	}
}

// ExpectedPGMajorInt is ExpectedPGMajor parsed as an int.
func ExpectedPGMajorInt() int {
	n, _ := strconv.Atoi(ExpectedPGMajor())
	return n
}

// pgImage returns the postgres image to launch for the configured major.
func pgImage() string {
	return "postgres:" + ExpectedPGMajor() + "-alpine"
}

// StartPostgres launches a postgres container, waits for readiness,
// and returns its connection details. The container is shut down via
// t.Cleanup so tests don't have to manage lifecycle.
//
// Defaults:
//   - image: postgres:<major>-alpine where <major> comes from
//     PG_HARDSTORAGE_TEST_PG_MAJOR (default 17) — see pgImage
//   - user / pass / db: hsctl / hsctl / hsctl
//   - wal_level=replica so BASE_BACKUP works
//   - max_wal_senders=10
//   - 90-second startup budget (cold image pull on first run)
func StartPostgres(t *testing.T) *Postgres {
	return startPostgres(t, nil)
}

// StartPostgresWithInitdbArgs is StartPostgres plus custom initdb
// arguments, passed through to the image entrypoint via
// POSTGRES_INITDB_ARGS. Use it to exercise non-default cluster geometry
// that can only be set at initdb time — e.g. a non-16 MiB WAL segment
// size via "--wal-segsize=64" (the value pg_hardstorage's streamer
// refuses). The args string is space-separated, exactly as initdb
// expects (e.g. "--wal-segsize=64").
func StartPostgresWithInitdbArgs(t *testing.T, initdbArgs string) *Postgres {
	return startPostgres(t, map[string]string{"POSTGRES_INITDB_ARGS": initdbArgs})
}

func startPostgres(t *testing.T, extraEnv map[string]string) *Postgres {
	t.Helper()
	SkipIfNoDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	image := pgImage()
	opts := []testcontainers.ContainerCustomizer{
		tcpostgres.WithDatabase("hsctl"),
		tcpostgres.WithUsername("hsctl"),
		tcpostgres.WithPassword("hsctl"),
		// Enable replication so BASE_BACKUP / START_REPLICATION work
		// in the same container — we use this for both regular and
		// replication-mode tests.
		//
		// We DO NOT pass --auth-host=md5 to initdb.  Why: with a
		// custom config-file (WithConfigFile below), the entrypoint
		// boots PG with our conf which lacks password_encryption.
		// PG 17 defaults to scram-sha-256.  initdb's --auth-host=md5
		// flag sets pg_hba.conf to require md5, but the password
		// stored in pg_authid is SCRAM-hashed (per the runtime
		// password_encryption setting at the time POSTGRES_PASSWORD
		// is applied).  Result: pg_hba says md5, password is SCRAM
		// → "User does not have a valid SCRAM secret" / auth failure.
		// Sticking to PG's defaults (no auth-host override; SCRAM
		// in pg_hba; SCRAM-stored password) keeps the two halves
		// aligned and matches what every modern host uses anyway.
		tcpostgres.WithConfigFile(writeReplicationConf(t)),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60 * time.Second),
		),
	}
	// Custom initdb args (e.g. --wal-segsize) are baked at cluster
	// creation via POSTGRES_INITDB_ARGS — appended last so callers can
	// shape cluster geometry the default helper can't.
	if len(extraEnv) > 0 {
		opts = append(opts, testcontainers.WithEnv(extraEnv))
	}
	// Container startup goes through a bounded retry. Under heavy churn
	// (a soak creating/tearing down many PG containers back-to-back) the
	// testcontainers ryuk reaper can be mid-teardown — "unexpected
	// container status removing" / "wait for reaper ... detect internal
	// port" — when a fresh Run tries to register with it, failing startup
	// in under 3s with no fault in the container or the test. Retry those
	// transient reaper/port races; a real misconfiguration is not
	// transient and still fails fast on the final attempt.
	var container *tcpostgres.PostgresContainer
	var err error
	for attempt := 1; attempt <= 4; attempt++ {
		container, err = tcpostgres.Run(ctx, image, opts...)
		if err == nil || ctx.Err() != nil || !isTransientStartupErr(err) {
			break
		}
		t.Logf("testkit: start %s: transient infra error (attempt %d/4), retrying: %v", image, attempt, err)
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	if err != nil {
		t.Fatalf("testkit: start %s: %v", image, err)
	}
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := container.Terminate(shutdown); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("testkit: terminate postgres: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("testkit: get connection string: %v", err)
	}
	return &Postgres{Container: container, DSN: dsn}
}

// isTransientStartupErr reports whether a container-start error is a
// known-transient testcontainers/ryuk infra hiccup — the reaper being
// mid-teardown, or a reaper port-detection race — rather than a real
// misconfiguration. These clear on a quick retry; everything else should
// surface immediately so genuine breakage isn't masked behind retries.
func isTransientStartupErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "reaper") ||
		strings.Contains(s, "unexpected container status") ||
		strings.Contains(s, "detect internal port")
}

// writeReplicationConf emits a postgresql.conf turning on replication
// features required by BASE_BACKUP, START_REPLICATION, and logical
// decoding (the lag tests in internal/logical exercise the latter).
// `wal_level=logical` is a strict superset of `replica`, so physical-
// replication tests are unaffected.  Returned path is suitable for
// tcpostgres.WithConfigFile.
func writeReplicationConf(t *testing.T) string {
	t.Helper()
	conf := `# pg_hardstorage testkit — replication + logical decoding enabled
listen_addresses = '*'
wal_level = logical
max_wal_senders = 10
max_replication_slots = 10
hot_standby = on

# Pin segment 0 + early segments so the replication-stream tests
# can stream from the very start of the WAL.  Default wal_keep_size
# is 0 (no keep), and slots only pin WAL after restart_lsn updates;
# tests that create a slot then immediately stream from 0/0 race
# against the checkpoint that recycles segment 0.  64 MiB ≈ 4
# segments, more than enough for the test wall-clock budget.
wal_keep_size = 64MB

# PG requires min_wal_size >= 2 * wal_segment_size. The default
# (80MB) is fine for 16MB segments but rejects a cluster booted with
# a larger segment size (e.g. StartPostgresWithInitdbArgs
# --wal-segsize=64 needs >= 128MB). Pin a floor that satisfies every
# segment size the suite exercises; harmless for the 16MB default.
min_wal_size = 256MB
`
	// PG 17+ only: turn on the walsummarizer so every incremental-
	// backup scenario inherits the precondition.  Setting
	// summarize_wal on a pre-17 server would CRASH STARTUP with
	// "unrecognized configuration parameter", so the GUC is
	// version-gated and folded in only when ExpectedPGMajorInt()
	// reports 17+.
	if ExpectedPGMajorInt() >= 17 {
		conf += `
# PG 17+ incremental backups require the walsummarizer.  Cheap
# at idle; the cost is paid only when an incremental BASE_BACKUP
# actually consumes the summaries.
summarize_wal = on
`
	}
	dir := t.TempDir()
	path := dir + "/postgresql.conf"
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		t.Fatalf("testkit: write conf: %v", err)
	}
	return path
}
