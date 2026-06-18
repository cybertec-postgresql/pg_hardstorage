// local_docker.go — single-PG-in-Docker topology backed by
// testcontainers-go.
package topology

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	tc "github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
)

// localDockerTopology runs a single PG in a Docker container via
// testcontainers-go. The standard testcontainers PG module enables
// the WAL settings we need (`wal_level=logical`, `max_wal_senders=10`,
// `max_replication_slots=10`) and exposes a port mapping so a host
// process can connect.
//
// We don't pre-create any users or databases beyond the default
// `postgres` superuser — scenarios bring their own DDL through the
// load engine, and the testkit's job is to provide a clean PG.
type localDockerTopology struct {
	c   *tcpostgres.PostgresContainer
	dsn string
	// initialDSN is the DSN at Up time, kept as a fallback for
	// when the runtime ConnectionString call fails (container
	// transiently between stop+start).  ConnString() prefers a
	// fresh resolve so port changes across `docker start` (which
	// re-randomises tc's `0:5432` binding) are picked up.
	initialDSN string
}

func newLocalDocker() *localDockerTopology { return &localDockerTopology{} }

// Name returns "local-docker".
func (l *localDockerTopology) Name() string { return "local-docker" }

// init disables testcontainers-go's ryuk "reaper" sidecar at
// package-load time, BEFORE any tc client is constructed.  Under
// campaign-level Docker daemon contention (parallel soak + compat
// + scenario sweeps), ryuk creation races and surfaces as
// `reaper: new reaper: run container: started hook: wait until
// ready: ... unexpected container status "removing"` — blocking
// the whole scenario sweep.  Down() below already calls Terminate
// explicitly, so ryuk's only contribution (orphan cleanup on a
// crashed test process) is unnecessary here: a crashed run leaks
// at most one PG container, which the campaign-driver's
// `docker ps --filter name=...` sweep catches anyway.
//
// The testcontainers config (which decides whether to bootstrap
// ryuk) is cached after first read via a package-level sync.Once,
// so an in-Up() Setenv arrived too late for the SECOND and later
// scenarios within the same testkit process — exactly the failure
// mode the L2 scenario sweep surfaced. Package init runs once at
// program start; setEnvOnce preserves an operator's manual
// override (e.g. re-enabling ryuk for leak debugging).
func init() {
	_ = setEnvOnce("TESTCONTAINERS_RYUK_DISABLED", "true")
}

// Up starts a PG container via testcontainers, applies the
// scenario's PG-version / image / GUC overrides, waits for
// readiness, and remembers the DSN.  PG-17-only GUCs (such as
// summarize_wal) are version-gated against the resolved image
// tag.
func (l *localDockerTopology) Up(ctx context.Context, opts UpOptions) error {
	// Default to the current upstream-stable major. Single
	// source of truth lives in the pg package; bumping a
	// supported PG major doesn't require touching every
	// topology provider.
	image := fmt.Sprintf("postgres:%d", pg.MaxSupportedMajor)
	if opts.PGVersion != "" {
		image = "postgres:" + opts.PGVersion
	}
	// Explicit image override wins over the postgres:<N> default.
	// L4 scenarios that need a non-PG-PID-1 runtime (pg_upgrade)
	// or a multi-major image use this to swap the default
	// postgres:N for a custom build (see
	// dockerfiles/testbed/Dockerfile.multi-pg-l4).  When set,
	// the image MUST honour the same env-var contract the
	// official postgres image does (POSTGRES_DB / _USER /
	// _PASSWORD) so the tcpostgres options below don't need to
	// know about the swap.
	if opts.Image != "" {
		image = opts.Image
	}

	// PG 17+ incremental backups (BASE_BACKUP INCREMENTAL) refuse
	// to run unless `summarize_wal=on`, which switches on the
	// walsummarizer background worker.  We turn it on here so every
	// scenario that runs against a PG 17+ image inherits the
	// precondition without each YAML having to repeat it.
	//
	// CRITICAL: this GUC was introduced in PG 17.  Passing
	// `-c summarize_wal=on` to PG 15/16 CRASHES STARTUP with
	// "unrecognized configuration parameter \"summarize_wal\"",
	// so the flag is version-gated.  Custom images (opts.Image
	// set) bypass the gate — they are responsible for their own
	// GUCs, since we can't safely guess their PG major from a
	// freeform tag.
	cmdArgs := []string{
		"-c", "wal_keep_size=256MB",
		// wal_level=logical so scenarios can exercise logical
		// replication (CREATE_REPLICATION_SLOT ... LOGICAL fails
		// with SQLSTATE 55000 under the postgres:N default of
		// `replica`). `logical` is a strict superset of `replica`
		// — physical WAL-stream and backup scenarios are
		// unaffected — which is the same call pgtestkit's
		// writeReplicationConf already made for the integration
		// suite.
		"-c", "wal_level=logical",
	}
	if opts.Image == "" && imageMajor(image) >= 17 {
		// Default: enable WAL summarization on PG 17+ so the
		// incremental-backup scenarios inherit the precondition
		// by construction.  Scenarios that need a different
		// posture (specifically L3_incremental_summarize_wal_flip)
		// override via opts.ExtraGUCs below — `-c` flags later
		// in the argv WIN over earlier ones, so an override of
		// `summarize_wal=off` cleanly replaces this default.
		cmdArgs = append(cmdArgs, "-c", "summarize_wal=on")
	}
	// Scenario-level GUC overrides via `topology.extra_gucs`.
	// Sorted by key so the argv (and thus the resulting docker
	// run command) is deterministic across map iterations.
	if len(opts.ExtraGUCs) > 0 {
		keys := make([]string, 0, len(opts.ExtraGUCs))
		for k := range opts.ExtraGUCs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			cmdArgs = append(cmdArgs, "-c", fmt.Sprintf("%s=%s", k, opts.ExtraGUCs[k]))
		}
	}

	cnt, err := tcpostgres.Run(ctx, image,
		tcpostgres.WithDatabase("testkit"),
		tcpostgres.WithUsername("testkit"),
		tcpostgres.WithPassword("testkit"),
		// Enable replication-protocol features so scenarios can
		// drive backup/wal-stream commands against this PG.
		tc.WithEnv(map[string]string{
			"POSTGRES_INITDB_ARGS": "--data-checksums",
		}),
		// Keep ≥256MB of WAL on disk so a basebackup running
		// alongside heavy concurrent IO (soak testing's 4-wide
		// scenario parallelism, soak's 16-cell pool) doesn't
		// recycle a segment between BASE_BACKUP's start_lsn
		// snapshot and its trailing-WAL stream — which is what
		// surfaced as `requested WAL segment ... not found`
		// fails (Bug B in the soak aggregate report).  256MB ≈
		// 16 × 16MB segments, comfortably beyond what a 1GB
		// pgbench seed produces during basebackup.
		tc.WithCmdArgs(cmdArgs...),
		// Restart-on-failure: chaos scenarios (`inject: kind:
		// pg_kill`) intentionally signal PG to exit; without a
		// restart policy the container stays Exited and the
		// scenario can't continue.  `unless-stopped` means
		// docker brings PG back automatically on any abnormal
		// exit, but a clean Down() (via Terminate) still wins.
		// PG image's ENTRYPOINT handles initdb-skip on restart,
		// so subsequent boots reuse the existing datadir.
		tc.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.RestartPolicy = container.RestartPolicy{Name: container.RestartPolicyUnlessStopped}
		}),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return fmt.Errorf("local-docker: run %s: %w", image, err)
	}
	dsn, err := cnt.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = cnt.Terminate(ctx)
		return fmt.Errorf("local-docker: connection string: %w", err)
	}
	l.c = cnt
	l.dsn = dsn
	l.initialDSN = dsn
	return nil
}

// ConnString returns the current libpq DSN.  Re-resolves the host
// port on every call so a `docker start` after `docker kill` (which
// re-randomises tc's `0:5432` binding) is picked up by the next
// step.  Falls back to the cached initial DSN if the live resolve
// fails (container transiently between stop+start).
func (l *localDockerTopology) ConnString() string {
	if l.c == nil {
		return l.initialDSN
	}
	// Short-budget re-resolve; the docker SDK call is local
	// IPC, so a single-second cap is plenty and avoids hanging
	// the runner on a wedged docker daemon.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if dsn, err := l.c.ConnectionString(ctx, "sslmode=disable"); err == nil {
		l.dsn = dsn
		return dsn
	}
	return l.dsn
}

// Targets surfaces the testcontainers PG as a single inject.Target
// with role "pg".  The container ID (testcontainers' randomised
// short-name like `tc_xxxxxxx`) is what `docker exec` and
// `docker kill` need.  An `inject` step in a scenario can target
// it via `target=pg` (one-of-role match) or `target=pg_random`
// (single-element pick).
func (l *localDockerTopology) Targets() []inject.Target {
	if l.c == nil {
		return nil
	}
	return []inject.Target{
		&inject.DockerTarget{
			Container: l.c.GetContainerID(),
			RoleStr:   "pg",
		},
	}
}

// Down terminates the PG container.  Idempotent — a never-Up'd
// topology returns nil.
func (l *localDockerTopology) Down(ctx context.Context) error {
	if l.c == nil {
		return nil
	}
	err := l.c.Terminate(ctx)
	l.c = nil
	l.dsn = ""
	return err
}

// setEnvOnce sets key=val in the process environment if key is not
// already set. Used here so an operator who explicitly overrides
// TESTCONTAINERS_RYUK_DISABLED (e.g. to re-enable ryuk for debugging
// leaks) keeps their value. Returns the resulting value.
func setEnvOnce(key, val string) string {
	if cur, ok := os.LookupEnv(key); ok {
		return cur
	}
	_ = os.Setenv(key, val)
	return val
}

// imageMajor extracts the PG major from a `postgres:<tag>` image
// reference.  Accepts the common shapes: `postgres:17`,
// `postgres:17.2`, `postgres:17-alpine`, `postgres:17.2-bookworm`.
// Returns 0 for shapes we can't confidently parse (custom images,
// digest refs); callers MUST treat 0 as "skip version-gated GUCs".
func imageMajor(image string) int {
	// Split off the tag.  Bail on multi-colon refs (image:port/...).
	parts := strings.SplitN(image, ":", 2)
	if len(parts) != 2 {
		return 0
	}
	tag := parts[1]
	// Strip distro suffix (`17-alpine` → `17`) and patch level
	// (`17.2` → `17`).
	for _, sep := range []string{"-", "."} {
		if i := strings.Index(tag, sep); i >= 0 {
			tag = tag[:i]
		}
	}
	n, err := strconv.Atoi(tag)
	if err != nil {
		return 0
	}
	return n
}
