// Step handlers for the scenario runner.  This file owns the
// shell-out / fault-dispatch logic that was stubbed as
// "deferred to" in the v0.1 runner.
//
// runner.go owns the lifecycle (topology Up, load apply, scenario
// asserts, Down).  This file owns what each step kind DOES.
//
// Cross-step state — the repo URL, the deployment name, the last
// produced backup ID, the agent-binary path, the topology's
// inject targets — lives in runState.  runState is built once at
// the start of Run and threaded through runStep so a `restore`
// step can default to the `take_backup` step's backup ID, etc.

package runner

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/scenario"
)

// runState threads cross-step state through the scenario run.
//
// The repo + deployment are auto-provisioned per scenario at the
// first take_backup step, scoped to artefactDir so two parallel
// runs don't clobber each other.  agentBin is resolved at runner
// construction; targets come from the topology.
type runState struct {
	artefactDir string
	agentBin    string
	// namedBackups: take_backup steps with `name:` set populate
	// this map so a later restore step's `source:` can reference
	// the named ID instead of the auto-generated backup_id.  Used
	// by L4 upgrade scenarios that take >1 backup and need to
	// restore a specific earlier one.
	namedBackups map[string]string
	// pgConn is the DSN at scenario start.  Use ConnString() to
	// pick up post-switchover leader changes — pgConn is the
	// initial value, ConnString re-resolves through the
	// topology's leader-discovery on every call.
	pgConn     string
	connString func() string // always returns the CURRENT leader DSN
	deployment string
	repoURL    string
	repoInited bool
	lastBackup string

	// lastRestoreTarget is the target dir of the most-recent
	// successful `restore:` step.  Used by `restored_load`
	// when no explicit `name:` references a captured-state
	// entry; falls back to "" before any restore.
	lastRestoreTarget string
	loadFile          string // populated from scenario.Load.File at start of Run
	targets           inject.TargetSet

	// pgVersion is the scenario's topology pg_version (e.g. "17").
	// Threaded into runState because assert_restored_match spins up
	// its sandbox cluster from a `postgres:<version>` image and the
	// version must match the basebackup's PG version exactly — a
	// 17-format datadir can't be opened by a 16 binary.
	pgVersion string

	// walStreamCmd holds a running `pg_hardstorage wal
	// stream` invoked via the `wal_stream: { action: start
	// }` step.  Bracketed by a matching `action: stop` (or
	// runner teardown if the scenario forgets).  Re-resolves
	// the DSN at start so a stream that follows a Patroni
	// switchover hits the new leader; the streamer process
	// itself does not chase leader changes — operators who
	// need that should stop+start the stream around the
	// failover step.
	walStreamCmd    *exec.Cmd
	walStreamCancel context.CancelFunc
	walStreamDone   chan struct{}

	// agentEnv is merged into every `pg_hardstorage`
	// shell-out's environment.  Populated from the sink
	// runtime's EnvForAgent() — typically S3 / Azure
	// credentials the agent's storage layer needs.  Empty
	// when running against a file:// repo (the legacy
	// in-artefactDir layout that needs no creds).
	agentEnv map[string]string

	// capturedStates holds per-name scenario state captured
	// via `capture_lsn` and consumed by `restore: { to_lsn:
	// $name }` and `assert_restored_match: { name: ... }`.
	// Each entry threads three values across steps:
	//
	//   * LSN — what `restore --to-lsn` pins to;
	//   * ChecksumQuery + ChecksumLive — the SQL run against
	//     the live cluster at capture time + its result; the
	//     same query re-runs against the restored cluster
	//     for byte-equal comparison (closes the "did the
	//     replay match what was committed?" loop);
	//   * RestoredTarget — populated by the restore step that
	//     consumes this ref so assert_restored_match knows
	//     where to start the sandbox cluster.
	capturedStates map[string]*capturedState
}

// capturedState is the per-name struct stored in
// runState.capturedStates.  See the field comments on
// runState for cross-step semantics.
type capturedState struct {
	LSN            string
	ChecksumQuery  string
	ChecksumLive   string
	RestoredTarget string
}

// currentDSN returns the freshest DSN the topology can give us.
// After a Patroni switchover the previous leader has been
// demoted to a replica (read-only), so steps that issue writes
// MUST re-resolve through this rather than reusing pgConn.
func (s *runState) currentDSN() string {
	if s.connString != nil {
		if cur := s.connString(); cur != "" {
			return cur
		}
	}
	return s.pgConn
}

// resolveAgentBinary picks the pg_hardstorage binary the runner
// shells out to.  Order:
//
//  1. PG_HARDSTORAGE_BIN env var — operator-set explicit override.
//  2. ./bin/pg_hardstorage in the repo root (relative to runner
//     binary cwd) — what `make build` produces during local dev.
//  3. `pg_hardstorage` on PATH — the installed binary.
//
// Returns the resolved absolute path or a clear error so the
// scenario fails fast at runner construction rather than at the
// first take_backup.
func resolveAgentBinary() (string, error) {
	// canonicalise resolves the host path AFTER following any
	// symlinks.  Critical for the assert_restored_match sandbox:
	// PG's restore_command (embedded in postgresql.auto.conf by
	// the agent) records the canonical path that /proc/self/exe
	// returns inside the agent process.  If the testkit's bind-
	// mount target uses a SYMLINKED ALIAS (e.g. /home/foo →
	// /data/foo bind/symlink on this host), the bind-mount lands
	// at /home/... but PG's exec hunts for /data/... and `sh`
	// reports `command not found`, killing recovery.  Resolving
	// here makes every downstream use of state.agentBin agree
	// with what the agent will embed.
	canonicalise := func(p string) string {
		if c, cerr := filepath.EvalSymlinks(p); cerr == nil {
			return c
		}
		return p
	}
	if v := os.Getenv("PG_HARDSTORAGE_BIN"); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			return "", fmt.Errorf("PG_HARDSTORAGE_BIN: %w", err)
		}
		if _, err := os.Stat(abs); err != nil {
			return "", fmt.Errorf("PG_HARDSTORAGE_BIN=%q: %w", v, err)
		}
		return canonicalise(abs), nil
	}
	candidates := []string{"./bin/pg_hardstorage", "bin/pg_hardstorage"}
	for _, c := range candidates {
		if abs, err := filepath.Abs(c); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return canonicalise(abs), nil
			}
		}
	}
	if path, err := exec.LookPath("pg_hardstorage"); err == nil {
		return canonicalise(path), nil
	}
	return "", fmt.Errorf("pg_hardstorage binary not found (set PG_HARDSTORAGE_BIN or run from a tree with bin/pg_hardstorage)")
}

// runSeed bulk-loads PG to roughly target_gb of on-disk data
// via `pgbench -i -s <scale>`.  Scale = target_gb * 67
// (pgbench scale=1 ≈ 15 MB after PG overhead + indexes).
//
// Two execution paths, in order of preference:
//
//  1. **In-container** — for docker-backed topologies the
//     runner identifies the LEADER container by reverse-
//     mapping the topology's current DSN port through
//     `docker ps`, then runs `docker exec <leader>
//     su -l postgres -c "pgbench -i -s N -d postgres"`.
//     Removes the host-pgbench dependency and works against
//     Spilo (patroni-local-docker), DockerCellRuntime, and
//     any future docker-compose topology.
//
//  2. **Host fallback** — when no docker leader is
//     identifiable (DSN parse fails, `docker ps` errors, or
//     the topology is non-docker), the runner falls back to
//     host-side `pgbench` against the topology's DSN.  This
//     keeps SSH-style and bare-metal topologies working once
//     they land.
//
// An absent or non-positive target_gb is a step error: a
// `seed:` block with no size is the kind of typo we want to
// surface, not silently no-op.
func runSeed(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	if st.TargetGB <= 0 {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("seed: target_gb must be ≥1 (got %d)", st.TargetGB)}
	}
	dsn := state.currentDSN()
	if dsn == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "seed: topology returned empty DSN"}
	}
	scale := st.TargetGB * 67

	// Path 1 — in-container.  Reverse-map the topology DSN
	// to a docker container, then run pgbench INSIDE that
	// container against PG via TCP localhost.  We don't
	// switch UNIX users (testcontainers' postgres image
	// has only the `postgres` OS user, but PG roles like
	// `testkit` aren't OS users — `su -l testkit` fails);
	// running pgbench as docker-exec's default user (root)
	// works fine because pgbench just opens a TCP
	// connection.  Credentials come from the DSN.
	if container, err := findDockerLeaderContainer(ctx, dsn); err == nil && container != "" {
		emit(out, "step.seed.starting", map[string]any{
			"index": idx, "target_gb": st.TargetGB, "scale": scale,
			"via": "docker exec " + container,
		})
		u, perr := url.Parse(dsn)
		if perr != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("seed: parse DSN: %v", perr)}
		}
		username := u.User.Username()
		password, _ := u.User.Password()
		dbName := strings.TrimPrefix(u.Path, "/")
		if dbName == "" {
			dbName = "postgres"
		}
		if username == "" {
			username = "postgres"
		}

		pgBin, err := containerPGBin(ctx, container, "pgbench")
		if err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("seed: locate pgbench in %s: %v", container, err)}
		}
		// Build the shell command so PGPASSWORD lands as an env
		// var only inside the container's sh process — never
		// echoed in the docker argv (which would leak into ps
		// for any user with /proc visibility).  -h 127.0.0.1
		// targets PG via TCP loopback inside the container,
		// which works regardless of the image's pg_hba.conf
		// peer/socket auth setup.
		// `-I dtgvp` runs the init steps with G (server-side data
		// generation via generate_series) instead of the default
		// `-I dtcvp` which uses client-side COPY.  Server-side is
		// 5-10× faster on bulk seeds (pgbench scale=335 = ~33.5M
		// tuples in pgbench_accounts) and was the bottleneck on
		// L4_patroni_failover_loop / L4_rapid_failover_x5 which
		// timed out around 1% done on hosts under heavy
		// concurrent docker load.  See:
		//   https://www.postgresql.org/docs/current/pgbench.html
		// init steps:
		//   d=drop, t=create tables, g=generate (server-side),
		//   v=vacuum, p=primary keys
		shellCmd := fmt.Sprintf(
			"PGPASSWORD=%q %s/pgbench -i -I dtgvp -s %d -h 127.0.0.1 -p 5432 -U %s -d %s",
			password, pgBin, scale, shellQuote(username), shellQuote(dbName),
		)
		args := []string{"exec", container, "sh", "-c", shellCmd}
		cmd := exec.CommandContext(ctx, "docker", args...)
		var combined bytes.Buffer
		cmd.Stdout = &combined
		cmd.Stderr = &combined
		if err := cmd.Run(); err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("seed: docker exec pgbench (-i -s %d) on %s: %v (output: %s)",
					scale, container, err, truncate(combined.Bytes(), 512))}
		}
		emit(out, "step.seed.completed", map[string]any{
			"index": idx, "target_gb": st.TargetGB, "via": "container:" + container,
		})
		return StepResult{Index: idx, Kind: st.Kind, Pass: true,
			Message: fmt.Sprintf("seeded ~%d GB (pgbench scale=%d) in %s", st.TargetGB, scale, container)}
	}

	// Path 2 — host fallback.
	pgbench, err := exec.LookPath("pgbench")
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "seed: no in-container leader resolvable AND pgbench not on PATH " +
				"(install postgresql-client on the runner host, or check that docker ps " +
				"lists a container mapping the topology's DSN port)"}
	}
	emit(out, "step.seed.starting", map[string]any{
		"index": idx, "target_gb": st.TargetGB, "scale": scale, "via": "host pgbench",
	})
	// Same `-I dtgvp` rationale as the in-container path above.
	cmd := exec.CommandContext(ctx, pgbench, "-i", "-I", "dtgvp", "-s", strconv.Itoa(scale), dsn)
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	if err := cmd.Run(); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("seed: pgbench -i -s %d: %v (output: %s)",
				scale, err, truncate(combined.Bytes(), 512))}
	}
	emit(out, "step.seed.completed", map[string]any{
		"index": idx, "target_gb": st.TargetGB, "via": "host",
	})
	return StepResult{Index: idx, Kind: st.Kind, Pass: true,
		Message: fmt.Sprintf("seeded ~%d GB (pgbench scale=%d, host)", st.TargetGB, scale)}
}

// findDockerLeaderContainer reverse-maps the topology's DSN
// host:port to a docker container that publishes that port to
// 5432 internally.  Returns ("", error) when the DSN can't be
// parsed, docker is unavailable, or no container matches —
// signalling the caller to fall back to the host path.
//
// The match is on the suffix `:<port>->5432/tcp` so we don't
// false-match on e.g. ports that contain the leader port as a
// substring (15432 vs 5432).
func findDockerLeaderContainer(ctx context.Context, dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	port := u.Port()
	if port == "" {
		return "", errors.New("DSN has no explicit port")
	}
	cmd := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Names}}\t{{.Ports}}")
	stdout, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("docker ps: %w", err)
	}
	needle := ":" + port + "->5432/tcp"
	for _, line := range strings.Split(string(stdout), "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) > 0 && fields[0] != "" {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no container maps :%s -> 5432/tcp", port)
}

// shellQuote wraps s in single quotes for safe inclusion in a
// `sh -c "..."` argv.  Embedded single quotes get
// '\” escaped so role names with apostrophes (rare but
// possible) don't break the shell parse.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// containerPGBin runs the same probe order as
// dockerfiles/testbed/entrypoint-pg.sh inside the container,
// adding /usr/lib/postgresql/*/bin first which is where Spilo
// and pgdg-apt land.  Returns the directory holding the
// requested binary, or an error wrapping the docker output.
func containerPGBin(ctx context.Context, container, binName string) (string, error) {
	probe := `for d in /usr/lib/postgresql/*/bin /usr/pgsql-*/bin /usr/bin; do ` +
		`if [ -x "$d/` + binName + `" ]; then echo "$d"; exit 0; fi; ` +
		`done; exit 1`
	cmd := exec.CommandContext(ctx, "docker", "exec", container, "bash", "-c", probe)
	stdout, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("locate %s: %w", binName, err)
	}
	return strings.TrimSpace(string(stdout)), nil
}

// runCaptureLSN records the live cluster's
// pg_current_wal_lsn() under the supplied name so a later
// restore step can PITR back to that exact point.  When the
// step also sets a checksum_query, the same query runs on
// the live cluster and its result is stashed for the
// matching assert_restored_match to compare against.
//
// Absent or duplicate names fail the step rather than
// silently overwriting — both are likely authoring
// mistakes.
func runCaptureLSN(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	if strings.TrimSpace(st.Name) == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "capture_lsn: name is required"}
	}
	if state.capturedStates == nil {
		state.capturedStates = map[string]*capturedState{}
	}
	if _, exists := state.capturedStates[st.Name]; exists {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("capture_lsn: name %q already captured (rename or remove the duplicate)", st.Name)}
	}
	dsn := state.currentDSN()
	if dsn == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "capture_lsn: topology returned empty DSN"}
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("capture_lsn: open: %v", err)}
	}
	defer db.Close()
	var lsn string
	if err := db.QueryRowContext(ctx, "select pg_current_wal_lsn()::text").Scan(&lsn); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("capture_lsn: query lsn: %v", err)}
	}
	cap := &capturedState{LSN: lsn}
	// Optional checksum capture.  Run it inside the same
	// transaction-window we read pg_current_wal_lsn() in by
	// reusing the same connection — minimises drift between
	// the LSN and the live state we're locking down.
	if cs := strings.TrimSpace(st.ChecksumQuery); cs != "" {
		var live string
		if err := db.QueryRowContext(ctx, cs).Scan(&live); err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("capture_lsn: checksum_query: %v", err)}
		}
		cap.ChecksumQuery = cs
		cap.ChecksumLive = live
	}
	state.capturedStates[st.Name] = cap

	body := map[string]any{"index": idx, "name": st.Name, "lsn": lsn}
	if cap.ChecksumLive != "" {
		body["checksum_live"] = cap.ChecksumLive
	}
	emit(out, "step.capture_lsn.completed", body)

	msg := fmt.Sprintf("captured %s = %s", st.Name, lsn)
	if cap.ChecksumLive != "" {
		msg += " (+ checksum)"
	}
	return StepResult{Index: idx, Kind: st.Kind, Pass: true, Message: msg}
}

// resolveToLSN turns the user-facing to_lsn value into a
// concrete LSN.  Empty in → empty out (caller skips the
// flag).  A "$name" prefix is looked up in capturedStates;
// literals pass through unchanged.  Errors fail the calling
// step rather than degrading silently — a typo in a $-ref
// should never silently pin to the most-recent backup.
func resolveToLSN(input string, state *runState) (string, error) {
	if input == "" {
		return "", nil
	}
	if !strings.HasPrefix(input, "$") {
		return input, nil
	}
	name := strings.TrimPrefix(input, "$")
	if state.capturedStates == nil {
		return "", fmt.Errorf("to_lsn references $%s but no capture_lsn step ran", name)
	}
	cs, ok := state.capturedStates[name]
	if !ok {
		return "", fmt.Errorf("to_lsn references unknown name $%s (captured: %v)", name, sortedCapturedKeys(state.capturedStates))
	}
	return cs.LSN, nil
}

// sortedCapturedKeys returns the keys of capturedStates in
// sorted order, used for stable error messages.
func sortedCapturedKeys(m map[string]*capturedState) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// runAssertRestoredMatch starts the restored datadir on a
// sandbox port, re-runs the checksum query that was captured
// at capture_lsn time, and fails the step if the result
// differs from the live checksum captured then.
//
// This is the byte-equal correctness check that closes the
// PITR loop: pg_verifybackup proves the manifest is intact
// against the restored bytes, but only this step proves the
// REPLAYED state matches what was actually committed at the
// captured LSN.
//
// Host-side PG is required (we shell out to pg_ctl + psql).
// On a host without postgresql installed the step skips
// gracefully with a warning event so a minimal CI run
// doesn't fail purely on this dependency.
//
// The sandbox cluster runs on 127.0.0.1:<base>+idx so two
// scenarios running in parallel against the same host don't
// collide.  The restored target dir is restored from
// capturedStates[name].RestoredTarget; the matching
// `restore: { to_lsn: $name }` step had to have populated
// it.
func runAssertRestoredMatch(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	if strings.TrimSpace(st.Name) == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "assert_restored_match: name is required"}
	}
	cs, ok := state.capturedStates[st.Name]
	if !ok || cs == nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("assert_restored_match: no capture_lsn captured for %q", st.Name)}
	}
	if cs.ChecksumQuery == "" || cs.ChecksumLive == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("assert_restored_match: capture_lsn { name: %s } did not run with a checksum_query", st.Name)}
	}
	if cs.RestoredTarget == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("assert_restored_match: no restore step consumed to_lsn: $%s yet", st.Name)}
	}

	// Sandbox runs in a docker container — no host pg_ctl
	// dependency, no host postgresql-* package required.  The
	// testkit's contract is "all PG infra lives in containers";
	// this gate honours that contract end-to-end.  See the
	// detailed rationale in the runRestoredSandbox docstring.
	if state.pgVersion == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "assert_restored_match: scenario.topology.pg_version is empty " +
				"(needed to pick the postgres:<version> image for the sandbox)"}
	}
	sandboxName := fmt.Sprintf("pg-hs-restored-sandbox-%d-%d", idx,
		time.Now().UnixNano()%1_000_000)
	logFile := filepath.Join(state.artefactDir,
		fmt.Sprintf("restored-cluster-%d.log", idx))

	// Sandbox PG runs against the restored datadir, which carries
	// the testbed's pg_authid — only the role that bootstrapped
	// initdb (typically `testkit`) exists there.  The default PG
	// docker image's `postgres` superuser does NOT.  Parse the
	// scenario's DSN once so docker exec ... psql -U <user> uses
	// the matching role.
	sandboxUser := pgUserFromDSN(state.pgConn)
	if sandboxUser == "" {
		sandboxUser = "postgres" // fall back; matches the official image's default
	}
	stopSandbox, startErr := startRestoredSandbox(ctx, sandboxName,
		cs.RestoredTarget, state.pgVersion,
		state.agentBin, state.repoURL, state.agentEnv)
	if startErr != nil {
		// Capture docker logs for forensics — the sandbox name
		// may already be gone if `docker run` itself bombed,
		// but `docker logs` is best-effort.
		logsOut, _ := exec.Command("docker", "logs", "--tail", "200",
			sandboxName).CombinedOutput()
		_ = os.WriteFile(logFile, logsOut, 0o644)
		// Best-effort cleanup of a half-spawned container.
		_ = exec.Command("docker", "rm", "-f", sandboxName).Run()
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("assert_restored_match: sandbox start: %v (logs: %s)",
				startErr, logFile)}
	}
	defer stopSandbox()

	// Poll for PG ready inside the container.  No host psql
	// needed — `docker exec` runs the bundled psql against the
	// sandbox's local socket / 127.0.0.1.
	//
	// 90s budget (was 30s).  The sandbox does crash-recovery
	// of every WAL segment between the basebackup's start and
	// the captured LSN, plus the postgres image's entrypoint
	// chmod / pg_hba dance.  Heavy scenarios (DDL storm with
	// 200k+ post-backup inserts, 3GB seed) replay 100s of MB
	// of WAL — 30s wasn't enough.  Concurrent campaign load
	// stretches it further.
	//
	// 180 (was 90) for the same reason
	// internal/restore/postverify/postverify.go bumped its
	// StartTimeout 60→180 earlier: under heavy
	// docker-daemon contention (concurrent soak +
	// compat + k8s + go-tests in parallel), a freshly-
	// restored cluster spends 10-30s on initdb-shaped
	// fsync + 30-90s walking the embedded WAL slice before
	// it accepts connections.  Soak testing hit this in
	// test-wal-stream-suite's L3 scenario at iter=10 — same
	// root cause, same fix.  pg_isready's poll cycle is
	// 1s so the wait collapses the moment the cluster
	// answers; no-load runs aren't visibly slower.
	// Default 180s; a streaming scenario that replays tens of GiB of
	// post-backup WAL (single-threaded recovery) needs far more and
	// sets `ready_timeout:` on the step.
	sandboxReadyTimeout := 180 * time.Second
	if st.ReadyTimeout != "" {
		if d, perr := time.ParseDuration(st.ReadyTimeout); perr == nil && d > 0 {
			sandboxReadyTimeout = d
		}
	}
	ready := false
	deadline := time.Now().Add(sandboxReadyTimeout)
	for time.Now().Before(deadline) {
		// Probe by asking PG whether it's STILL IN RECOVERY.
		// A bare `select 1` succeeds the moment the cluster
		// enters hot-standby mode, but the cluster is still
		// replaying WAL and will transition through "promoting"
		// (which BRIEFLY refuses connections with `the database
		// system is in recovery mode`) before reaching the
		// promoted state.  The next operator query lands in that
		// window and fails — surfaced by
		// L8_backend_offline_chain_recovery in suite-load runs
		// where sink reachability during restore_command made the
		// recovery walk slow enough that the checksum query
		// raced the promotion handoff.
		//
		// pg_is_in_recovery() returns 'f' once the cluster has
		// promoted (recovery_target_action=promote in
		// postgresql.auto.conf drives this).  We grep the
		// trimmed stdout for `^f$` so the probe is robust to
		// the leading whitespace psql's `-A` mode strips and
		// to PG's `t`/`f` formatting.
		check := exec.CommandContext(ctx, "docker", "exec", sandboxName,
			"psql", "-U", sandboxUser, "-At", "-c", "select pg_is_in_recovery()")
		out, err := check.Output()
		if err == nil && strings.TrimSpace(string(out)) == "f" {
			ready = true
			break
		}
		select {
		case <-ctx.Done():
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: "assert_restored_match: ctx cancelled while waiting for sandbox"}
		case <-time.After(time.Second):
		}
	}
	if !ready {
		// Drop the sandbox's stderr into logFile so the operator
		// can see why postmaster never came up (auth?  bad
		// pg_hba?  WAL replay errors?).
		logsOut, _ := exec.Command("docker", "logs", "--tail", "200",
			sandboxName).CombinedOutput()
		_ = os.WriteFile(logFile, logsOut, 0o644)
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("assert_restored_match: sandbox did not accept connections within %s (log: %s)", sandboxReadyTimeout, logFile)}
	}

	// Re-run the captured query against the restored cluster.
	queryCmd := exec.CommandContext(ctx, "docker", "exec", sandboxName,
		"psql", "-U", sandboxUser, "-At", "-c", cs.ChecksumQuery)
	var queryOut, queryErr bytes.Buffer
	queryCmd.Stdout = &queryOut
	queryCmd.Stderr = &queryErr
	if err := queryCmd.Run(); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("assert_restored_match: psql checksum: %v (stderr: %s)",
				err, truncate(queryErr.Bytes(), 256))}
	}
	restored := strings.TrimSpace(queryOut.String())
	live := strings.TrimSpace(cs.ChecksumLive)

	if restored != live {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("assert_restored_match: MISMATCH for %q\n  live:     %s\n  restored: %s",
				st.Name, truncate([]byte(live), 200), truncate([]byte(restored), 200))}
	}

	emit(out, "step.assert_restored_match.completed", map[string]any{
		"index":    idx,
		"name":     st.Name,
		"checksum": restored,
	})
	return StepResult{Index: idx, Kind: st.Kind, Pass: true,
		Message: fmt.Sprintf("restored cluster matches live state at %s", st.Name)}
}

// pgUserFromDSN extracts the libpq username from a postgres://
// or postgresql:// URL DSN.  Returns "" if the DSN doesn't carry
// one or fails to parse — caller falls back to the docker
// postgres image's default ("postgres") in that case.
//
// libpq accepts both URI and key/value DSN forms; testkit
// scenarios always get the URI form from
// testcontainers-go/modules/postgres so a URL parse covers every
// real input.  A future provider that emits the key/value form
// would need a fuller libpq-DSN parser here, but YAGNI until then.
func pgUserFromDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return ""
	}
	return u.User.Username()
}

// startRestoredSandbox spins up a `postgres:<version>` docker
// container with the restored datadir bind-mounted at
// /var/lib/postgresql/data, returning a stop closure the caller
// defers and an error if the container failed to start.
//
// Why a container instead of host pg_ctl:
//
//  1. The testkit's contract is "all PG infra lives in
//     containers" — a host pg_ctl probe couples the strict gate
//     to whatever the operator has installed locally and makes
//     air-gapped CI hosts depend on system packages.
//  2. Cross-version: scenarios run pg_version=15..18 on the
//     same host.  pg_ctl 16 cannot start a 17-format datadir;
//     pulling postgres:<version> per scenario sidesteps the
//     mismatch entirely.
//  3. Determinism: the postgres:<version> image is pinned
//     (Debian-based, single point release per major) and
//     reproducible across machines.  A host install drifts
//     with apt-get update.
//
// Implementation notes:
//
//   - --user $(host_uid):$(host_gid): the restored datadir is
//     written by the agent shell-out running as the host user,
//     so the container's PG must run as that same UID to read
//     and write the bind-mounted dir.  PG's own data-dir
//     ownership check is satisfied because container UID ==
//     dir owner UID by construction.
//   - listen_addresses=*: required so docker exec ... psql -h
//     127.0.0.1 reaches postmaster.  pg_hba.conf in the
//     restored datadir governs auth — testbed PG datadirs ship
//     with localhost trust so postgres-as-postgres-from-localhost
//     works.
//   - logging_collector=off: pushes postmaster stderr to docker
//     logs, where the caller's failure path scrapes it via
//     `docker logs --tail` for the artefact directory.
//   - --rm: the container vanishes on `docker stop`, which the
//     caller's defer triggers.  No manual cleanup needed; even
//     a panic on the runner side leaves no stranded containers.
//
// Image pull:  `docker run` will fetch postgres:<version> on
// first use if it isn't local.  An air-gapped CI host should
// pre-pull via `pg_hardstorage_testkit image pull-pg
// --version <N>` (or equivalent) before running scenarios; on
// connected hosts the pull is transparent.
func startRestoredSandbox(ctx context.Context, name, datadir, version,
	agentBin, repoURL string, agentEnv map[string]string,
) (func(), error) {
	absDatadir, err := filepath.Abs(datadir)
	if err != nil {
		return nil, fmt.Errorf("absolute path of %q: %w", datadir, err)
	}
	// Deliberately NO --rm: a sandbox that exits before docker
	// exec can land its first poll (auth failure / WAL replay
	// abort / chown miss) would otherwise be auto-removed and
	// `docker logs` afterwards would return "no such container",
	// which is the exact failure mode that wasted iteration
	// cycles during the L3 sandbox bring-up.  The stop closure
	// below uses `docker rm -f` so cleanup still happens, and a
	// caller that wants logs has a window to grab them.
	// `:z` (lowercase, SELinux shared label) on every bind-
	// mount: SELinux-enforcing hosts (Fedora, RHEL, Alma,
	// Rocky) deny the container's reads of host-labelled files
	// (`tmp_t` / `user_home_t`) even when the container's
	// numeric UID matches the file owner.  Without this, the
	// postgres image's entrypoint trips on
	//
	//   chmod: changing permissions of '/var/lib/postgresql/data': Permission denied
	//   ...
	//   FATAL:  could not open configuration file "...postgresql.conf": Permission denied
	//
	// The fix mirrors what compat_archive.go and the sink/
	// minio bind-mounts already do; missing it here meant
	// every assert_restored_match step on a SELinux host
	// failed at the sandbox's first read.  No-op on systems
	// without SELinux.
	// Pick a free port dynamically so multiple concurrent
	// assert_restored_match sandboxes (one per concurrent
	// scenario) don't collide on the host's 5432.  --network=host
	// (used below for sink-backed scenarios) shares the host's
	// network namespace, so two sandboxes both listening on 5432
	// would race for the bind() and one would fail with
	// `could not bind IPv4 address "0.0.0.0": Address already in
	// use` — observed on 2026-05-09 when 4 parallel sweep slots
	// reached the assert step at the same time.
	//
	// The port is forwarded into the container via PGPORT so the
	// caller's `docker exec ... psql` finds the right unix
	// socket (PG's socket name encodes the port:
	// `.s.PGSQL.<port>`).
	pgPort, err := pickFreePort()
	if err != nil {
		return nil, fmt.Errorf("pick sandbox port: %w", err)
	}
	args := []string{
		"run", "-d",
		"--name", name,
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-v", absDatadir + ":/var/lib/postgresql/data:z",
		"-e", "PGDATA=/var/lib/postgresql/data",
		"-e", fmt.Sprintf("PGPORT=%d", pgPort),
	}

	// The agent's restore embeds a literal restore_command of the
	// form `<agentBin> wal fetch <deployment> %f %p --repo
	// <repoURL>` in the restored postgresql.auto.conf.  PG runs
	// it during recovery to fetch each WAL segment.  Bind-mount
	// the agent binary at the SAME absolute path so the embedded
	// command resolves inside the sandbox.  Without this, PG
	// reports "could not restore file ... command not found" and
	// startup aborts before accepting connections.
	// `:ro,z` on the agent + repo mounts (and a plain `:z` on
	// the data dir above): same SELinux MCS-label rationale as
	// the data-dir mount.  PG's embedded restore_command
	// inside the sandbox shells out to `<agentBin> wal fetch
	// ... --repo file://<repoPath>` — both paths must be
	// readable by the container even on SELinux-enforcing
	// hosts.  No-op on non-SELinux systems.
	if agentBin != "" {
		absAgent, err := filepath.Abs(agentBin)
		if err == nil {
			// Match the canonical path the agent embeds in
			// restore_command — see resolveAgentBinary for the
			// rationale.  Without EvalSymlinks here, a tree
			// rooted under a symlinked alias (e.g. /home/foo →
			// /data/foo) makes the bind-mount land at one path
			// while PG's recovery hunts for the other, and the
			// sandbox aborts with `sh: ...: not found`.
			if canonical, cerr := filepath.EvalSymlinks(absAgent); cerr == nil {
				absAgent = canonical
			}
			// PG_HARDSTORAGE_SANDBOX_BIN supplies a separate
			// binary to bind-mount AT the host agent's canonical
			// path inside the sandbox container.  Use case:
			// macOS dev host (darwin/arm64) running a Linux PG
			// container — the host binary the runner shells out
			// to is darwin, but PG inside the container expects
			// a Linux ELF when restore_command fires.  The env
			// var lets a developer cross-build the Linux variant
			// (`GOOS=linux GOARCH=<arch> go build -o
			// bin/pg_hardstorage-linux-<arch> ./cmd/pg_hardstorage`)
			// and point this at it.  No effect on Linux CI where
			// the host binary IS a Linux ELF — the env var stays
			// unset and the host binary is mounted directly.
			//
			// The mount TARGET is still the agent's canonical
			// host path (what got baked into restore_command at
			// backup time); only the SOURCE changes.
			source := absAgent
			if alt := strings.TrimSpace(os.Getenv("PG_HARDSTORAGE_SANDBOX_BIN")); alt != "" {
				if absAlt, aerr := filepath.Abs(alt); aerr == nil {
					if canonical, cerr := filepath.EvalSymlinks(absAlt); cerr == nil {
						source = canonical
					} else {
						source = absAlt
					}
				}
			}
			args = append(args, "-v", source+":"+absAgent+":ro,z")
		}
	}

	// file:// repos: bind-mount the repo dir at the same host
	// path so wal fetch can read it from inside the sandbox.
	// Other URL schemes (s3://, azblob://, gcs://, sftp://) reach
	// their backends over the network — for sink-backed
	// scenarios we attach to host networking + forward the
	// agent's env so the SDK's credential chain finds its keys.
	if strings.HasPrefix(repoURL, "file://") {
		repoPath := strings.TrimPrefix(repoURL, "file://")
		if absRepo, aerr := filepath.Abs(repoPath); aerr == nil {
			// Same symlink-canonicalisation as the agent
			// binary above: the agent records the repo URL
			// in its restore_command using its own
			// understanding of the path, which is the
			// resolved one.  Mismatch breaks `wal fetch
			// --repo <url>` inside the sandbox.
			if canonical, cerr := filepath.EvalSymlinks(absRepo); cerr == nil {
				absRepo = canonical
			}
			args = append(args, "-v", absRepo+":"+absRepo+":ro,z")
		}
	} else {
		// Sink-backed: share the host's network so the
		// agent (running inside the sandbox) can reach the
		// sink emulator's port-forwarded endpoint at
		// 127.0.0.1:<port> exactly like the host-side
		// agent does.
		args = append(args, "--network=host")
		for k, v := range agentEnv {
			args = append(args, "-e", k+"="+v)
		}
	}

	args = append(args,
		"postgres:"+version,
		"-c", fmt.Sprintf("port=%d", pgPort),
		"-c", "listen_addresses=*",
		"-c", "logging_collector=off",
		// Override shared_preload_libraries.  Source clusters
		// running custom images (Spilo's bg_mon, pg_partman,
		// pg_stat_statements_extra, ...) carry these as
		// shared_preload_libraries entries in postgresql.conf;
		// the bare `postgres:<version>` image lacks the .so
		// files and PG aborts at startup with "could not access
		// file 'bg_mon': No such file or directory".  The
		// sandbox only re-runs a checksum query — none of these
		// libraries are needed; clearing the list makes the
		// sandbox start regardless of what the source loaded.
		"-c", "shared_preload_libraries=",
		// Same logic for archive_command / restore_library /
		// other side-channel hooks that depend on host paths
		// not present in the sandbox.  We leave restore_command
		// alone — that one is what makes recovery work via the
		// agent binary bind-mount.
		"-c", "archive_mode=off",
		// SSL: Spilo (and other custom images) ships postgresql.
		// conf with `ssl=on` and ssl_cert_file pointing at host
		// paths like /run/certs/server.crt that the bare image
		// doesn't carry.  Disable SSL so the sandbox starts
		// regardless of how the source's TLS chain is wired —
		// the assert step only runs over docker exec on the
		// container's loopback, no transport security needed.
		"-c", "ssl=off",
		// Spilo postgresql.conf hard-codes hba_file +
		// ident_file at /home/postgres/pgdata/pgroot/data/pg_*.conf.
		// Inside the sandbox, PGDATA is bind-mounted at
		// /var/lib/postgresql/data and the pg_hba.conf /
		// pg_ident.conf files live there too — but the absolute
		// Spilo path doesn't exist.  PG would abort at startup
		// with "could not load /home/postgres/pgdata/.../pg_hba.conf".
		// Point both at the standard PGDATA-relative defaults.
		"-c", "hba_file=/var/lib/postgresql/data/pg_hba.conf",
		"-c", "ident_file=/var/lib/postgresql/data/pg_ident.conf",
	)
	out, runErr := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if runErr != nil {
		return nil, fmt.Errorf("docker run postgres:%s: %w (output: %s)",
			version, runErr, truncate(out, 384))
	}
	stop := func() {
		// rm -f handles both running and exited containers in
		// one call.  Fresh context — parent ctx may already be
		// cancelled (scenario timeout, user Ctrl-C) and we
		// still want the container gone.
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", name).Run()
	}
	return stop, nil
}

// runWALStream starts or stops a backgrounded `pg_hardstorage
// wal stream` against the current leader.  Step shape:
//
//   - wal_stream: { action: start }
//   - wal_stream: { action: stop }
//
// `start` requires the repo to be initialised first — the
// streamer expects a writable repo at the URL it's pointed at.
// The handler triggers an idempotent repo init before forking
// the streamer so scenario authors don't have to model that.
//
// `stop` is idempotent against missing state — calling stop
// without a prior start is a step-success no-op so a scenario
// that runs cleanup on every path can call stop unconditionally.
func runWALStream(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	switch strings.ToLower(strings.TrimSpace(st.Action)) {
	case "start":
		if state.walStreamCmd != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: "wal_stream start: already running"}
		}
		if !state.repoInited {
			if err := initRepo(ctx, state, out); err != nil {
				return StepResult{Index: idx, Kind: st.Kind, Pass: false,
					Message: fmt.Sprintf("wal_stream start: repo init: %v", err)}
			}
			state.repoInited = true
		}
		dsn := state.currentDSN()
		if dsn == "" {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: "wal_stream start: topology returned empty DSN"}
		}
		bgCtx, cancel := context.WithCancel(context.Background())
		cmd := state.agentCmd(bgCtx,
			"wal", "stream", state.deployment,
			"--pg-connection", dsn,
			"--repo", state.repoURL)
		// Capture combined output to the artefact dir so a
		// scenario failure points at exactly what the streamer
		// said before dying.
		logPath := filepath.Join(state.artefactDir,
			fmt.Sprintf("wal-stream-%d.log", idx))
		f, ferr := os.Create(logPath)
		if ferr != nil {
			cancel()
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("wal_stream start: open log %s: %v", logPath, ferr)}
		}
		cmd.Stdout = f
		cmd.Stderr = f
		if err := cmd.Start(); err != nil {
			_ = f.Close()
			cancel()
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("wal_stream start: %v", err)}
		}
		state.walStreamCmd = cmd
		state.walStreamCancel = cancel
		state.walStreamDone = make(chan struct{})
		go func() {
			_ = cmd.Wait()
			_ = f.Close()
			close(state.walStreamDone)
		}()
		emit(out, "step.wal_stream.started", map[string]any{
			"index": idx,
			"log":   logPath,
		})
		return StepResult{Index: idx, Kind: st.Kind, Pass: true,
			Message: "wal stream started, log: " + logPath}
	case "stop", "":
		if state.walStreamCmd == nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: true,
				Message: "wal stream not running (no-op)"}
		}
		// Graceful stop: SIGTERM lets the streamer's signal
		// handler run pg_switch_wal and wait for the in-flight
		// segment to flush, so PITR targets in the most recent
		// segment stay reachable.  Force-kill via the original
		// context-cancel only if the streamer doesn't exit
		// within a wall-clock budget.
		gracefulMode := "graceful_sigterm"
		if state.walStreamCmd.Process != nil {
			if err := state.walStreamCmd.Process.Signal(syscall.SIGTERM); err != nil {
				gracefulMode = "sigterm_failed_falling_back_to_cancel"
				state.walStreamCancel()
			}
		} else {
			gracefulMode = "no_process_handle_falling_back_to_cancel"
			state.walStreamCancel()
		}
		select {
		case <-state.walStreamDone:
		case <-time.After(15 * time.Second):
			// Timed out; force-kill via context cancel.
			gracefulMode = "graceful_timeout_force_killed"
			state.walStreamCancel()
			<-state.walStreamDone
		}
		state.walStreamCmd = nil
		state.walStreamCancel = nil
		state.walStreamDone = nil
		emit(out, "step.wal_stream.stopped", map[string]any{
			"index": idx,
			"mode":  gracefulMode,
		})
		return StepResult{Index: idx, Kind: st.Kind, Pass: true,
			Message: "wal stream stopped (" + gracefulMode + ")"}
	default:
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("wal_stream: unknown action %q (want start | stop)", st.Action)}
	}
}

// runWaitWALArchived blocks until the repo holds committed WAL whose
// EndLSN reaches the step's to_lsn target — i.e. every WAL byte up to
// that LSN is durably archived, so a `restore --to-lsn` would find it.
//
// Why a streaming-backup scenario needs this: the WAL streamer is an
// ASYNCHRONOUS archiver. After a heavy write burst (VACUUM FULL, bulk
// load) PG can emit WAL faster than the streamer archives it; the
// streamer then drains the backlog from the slot over the following
// minutes. Stopping the streamer — and asserting a restore — before it
// has caught up tests an instant-RPO the async model never promised,
// and fails spuriously. This step gives the streamer the time the
// model assumes. The streamer MUST still be running: place this step
// BEFORE `wal_stream: { action: stop }`.
func runWaitWALArchived(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	target, err := resolveToLSN(st.ToLSN, state)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "wait_wal_archived: " + err.Error()}
	}
	if target == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "wait_wal_archived: to_lsn is required"}
	}
	targetLSN, err := pglogrepl.ParseLSN(target)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("wait_wal_archived: bad to_lsn %q: %v", target, err)}
	}
	const timeout = 20 * time.Minute
	deadline := time.Now().Add(timeout)
	var highest pglogrepl.LSN
	for {
		if h, herr := highestArchivedWAL(state.repoURL, state.deployment); herr == nil {
			highest = h
			if highest >= targetLSN {
				emit(out, "step.wait_wal_archived.completed", map[string]any{
					"index":        idx,
					"target_lsn":   target,
					"archived_lsn": highest.String(),
				})
				return StepResult{Index: idx, Kind: st.Kind, Pass: true,
					Message: fmt.Sprintf("WAL archived through %s (target %s)", highest, target)}
			}
		}
		if time.Now().After(deadline) {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("wait_wal_archived: streamer did not archive WAL up to %s "+
					"within %s (highest archived: %s) — the streamer is not keeping pace",
					target, timeout, highest)}
		}
		select {
		case <-ctx.Done():
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: "wait_wal_archived: ctx cancelled while waiting"}
		case <-time.After(2 * time.Second):
		}
	}
}

// highestArchivedWAL returns the maximum EndLSN across every committed
// segment manifest in the repo for the deployment. repoURL must be a
// file:// URL (the testkit's local-docker provider always uses one).
// A not-yet-created WAL directory returns (0, nil) — "nothing archived
// yet", not an error.
func highestArchivedWAL(repoURL, deployment string) (pglogrepl.LSN, error) {
	u, err := url.Parse(repoURL)
	if err != nil {
		return 0, err
	}
	walDir := filepath.Join(u.Path, "wal", deployment)
	if _, statErr := os.Stat(walDir); os.IsNotExist(statErr) {
		return 0, nil
	}
	var highest pglogrepl.LSN
	walkErr := filepath.Walk(walDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil // tolerate races with the streamer writing
		}
		if !strings.HasSuffix(path, ".json") || strings.Contains(path, ".tmp.") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		m, perr := walsink.ParseSegmentManifest(body)
		if perr != nil {
			return nil
		}
		if end, perr := pglogrepl.ParseLSN(m.EndLSN); perr == nil && end > highest {
			highest = end
		}
		return nil
	})
	return highest, walkErr
}

// runDropSlot drops a physical replication slot on the live
// primary, forcing the streamer's next reconnect into the
// EnsureSlot Strategy-C (recreate-with-RESERVE_WAL) branch.
//
// Why this is a scenario primitive: in production, slot loss
// happens when an admin runs pg_drop_replication_slot or when a
// new Patroni leader doesn't propagate the slot via
// permanent_slots.  The streamer's response is the same in
// either case (next reconnect's EnsureSlot detects the gap and
// recreates).  This step lets scenarios force that branch
// deterministically without needing a full Patroni cluster
// misconfiguration.
//
// Slot defaults to the canonical wal-stream name
// (pg_hardstorage_<deployment> with hyphens underscored).  An
// explicit `slot:` field in the step overrides.
//
// Active slots refuse to drop; scenarios must `wal_stream: stop`
// before this step.
func runDropSlot(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	slotName := strings.TrimSpace(st.Slot)
	if slotName == "" {
		safe := strings.ReplaceAll(state.deployment, "-", "_")
		slotName = "pg_hardstorage_" + safe
	}
	dsn := state.currentDSN()
	if dsn == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "drop_slot: topology returned empty DSN"}
	}
	c, err := pg.Connect(ctx, dsn, pg.ModeReplication)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("drop_slot: open replication conn: %v", err)}
	}
	defer c.Close(ctx)
	if err := replication.DropSlot(ctx, c, slotName); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("drop_slot %q: %v", slotName, err)}
	}
	emit(out, "step.drop_slot.completed", map[string]any{
		"index": idx, "slot": slotName,
	})
	return StepResult{Index: idx, Kind: st.Kind, Pass: true,
		Message: "dropped slot " + slotName}
}

// runTakeBackup shells out to `pg_hardstorage backup` and stashes
// the resulting backup_id in state for a later restore step to
// reference.  The repo is auto-initialised on the first call so
// scenario authors don't have to model the bootstrap.
func runTakeBackup(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	if !state.repoInited {
		if err := initRepo(ctx, state, out); err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("repo init: %v", err)}
		}
		state.repoInited = true
	}
	deployment := st.Deployment
	if deployment == "" {
		deployment = state.deployment
	}
	// Re-resolve DSN: a take_backup that runs after an inject
	// patroni_failover must hit the CURRENT primary, not the
	// scenario-start one (now a read-only replica).
	argv := []string{
		"backup", deployment,
		"--pg-connection", state.currentDSN(),
		"--repo", state.repoURL,
		// --include-wal makes the basebackup self-contained:
		// every WAL segment needed for recovery up to backup-end
		// LSN ships inside the tar stream.  Production deployments
		// rely on a `wal stream` sidecar instead, but a testkit
		// scenario without that sidecar (the L1-L3 fast gates)
		// would otherwise leave the restored datadir unable to
		// reach a consistent state at startup.  Always-on for
		// testkit-driven backups; the agent default is still off.
		"--include-wal",
		"-o", "json",
	}
	// PG 17+ incremental: resolve incremental_from against
	// scenario state and pass the resolved backup ID through to
	// the CLI's --incremental-from flag.  $LAST_BACKUP picks the
	// most recent take_backup; a bare name picks the named
	// backup the scenario captured earlier with `name:`.  An
	// unresolvable reference fails the scenario fast — silently
	// degrading to a full backup would let the test pass against
	// a broken implementation.
	if st.IncrementalFrom != "" {
		var parentID string
		switch st.IncrementalFrom {
		case "$LAST_BACKUP":
			if state.lastBackup == "" {
				return StepResult{Index: idx, Kind: st.Kind, Pass: false,
					Message: "incremental_from: $LAST_BACKUP referenced but no prior take_backup ran"}
			}
			parentID = state.lastBackup
		default:
			if state.namedBackups == nil || state.namedBackups[st.IncrementalFrom] == "" {
				return StepResult{Index: idx, Kind: st.Kind, Pass: false,
					Message: fmt.Sprintf("incremental_from: no named backup %q (set `name: %s` on an earlier take_backup)",
						st.IncrementalFrom, st.IncrementalFrom)}
			}
			parentID = state.namedBackups[st.IncrementalFrom]
		}
		argv = append(argv, "--incremental-from", parentID)
	}
	cmd := state.agentCmd(ctx, argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("backup: %v (stderr: %s)", err, truncate(stderr.Bytes(), 2048))}
	}
	// Same JSON-shape contract the testkit's docker runtime uses:
	// `{"result": {"backup_id": "..."}}`.  encoding/json over a
	// narrow struct so the match survives whitespace + extra
	// fields the agent might add.
	var parsed struct {
		Result struct {
			BackupID string `json:"backup_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("parse backup output: %v", err)}
	}
	if parsed.Result.BackupID == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "backup completed but no backup_id in output"}
	}
	state.lastBackup = parsed.Result.BackupID
	// `name:` on take_backup is OPTIONAL — when set, it stores the
	// resulting backup_id under that key in state.namedBackups so
	// later steps can reference it as `source: <name>` (e.g. the
	// L4 upgrade scenarios that need to restore TWO specific
	// backups taken at different points in the run).  Without
	// named-backups, a scenario that takes N backups can only
	// restore the most recent — which collapses any "old backup
	// still restorable" assertion.  No-op when Name is empty.
	if st.Name != "" {
		if state.namedBackups == nil {
			state.namedBackups = map[string]string{}
		}
		state.namedBackups[st.Name] = parsed.Result.BackupID
	}
	emit(out, "step.take_backup.completed", map[string]any{
		"index":     idx,
		"backup_id": parsed.Result.BackupID,
	})
	return StepResult{Index: idx, Kind: st.Kind, Pass: true,
		Message: "backup_id=" + parsed.Result.BackupID}
}

// runRestore shells out to `pg_hardstorage restore` into a fresh
// per-step subdir of artefactDir.  Backup ID defaults to the
// scenario's last take_backup; the operator can override with
// `source: <id>` or `source: latest` (which the agent CLI
// resolves through the repo).
//
// PITR knobs (To / ToCheckpoint) are passed through unchanged.
func runRestore(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	if !state.repoInited {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "restore step requires a prior take_backup (no repo initialised)"}
	}
	deployment := st.Deployment
	if deployment == "" {
		deployment = state.deployment
	}
	source := st.Source
	if source == "" {
		source = state.lastBackup
	}
	// Resolve named-backup references — when `source:` matches a
	// key written by an earlier `take_backup: { name: <key> }`,
	// substitute the recorded backup_id.  Lets L4 upgrade
	// scenarios reference specific backups by point-in-run name
	// (pre_swap, post_swap, …) rather than the auto-generated
	// scenario.full.<ts>.<hex> ID.  When source is already a
	// real backup_id (contains '.') the lookup is a no-op.
	if id, ok := state.namedBackups[source]; ok {
		source = id
	}
	if source == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "restore: no `source:` set and no prior take_backup to default to"}
	}
	target := filepath.Join(state.artefactDir, fmt.Sprintf("restore-step-%d", idx))
	args := []string{
		"restore", deployment, source,
		"--repo", state.repoURL,
		"--target", target,
		// The agent's postverify spins up PG via host pg_ctl,
		// which couples scenario success to the host's PG
		// install (and binds it to a specific major version).
		// The testkit's contract is "all PG infra in
		// containers" — assert_restored_match runs the
		// equivalent cluster-start gate from a
		// postgres:<version> container that matches the
		// scenario's pg_version exactly.  Disabling the
		// agent-side postverify here keeps the testkit
		// host-PG-free without losing the smoke gate.
		"--verify-restore", "off",
	}
	// PITR — runner mirrors the CLI's flag names so a scenario can
	// drop in any of the documented PITR knobs without a separate
	// schema map.
	if st.To != "" {
		args = append(args, "--to", st.To)
	}
	if st.ToCheckpoint != "" {
		// `to_checkpoint` in the YAML maps to `--to-name` on the
		// CLI (named restore-point), which is what scenarios
		// commonly mean by "checkpoint".
		args = append(args, "--to-name", st.ToCheckpoint)
	}
	// Record the restored target back onto the captured state
	// so a later assert_restored_match can find this datadir.
	// The lookup runs BEFORE the agent shells out — a typo'd
	// $-ref should fail before we burn time on a 10 GB restore.
	//
	// Two ways to establish the linkage:
	//   * `to_lsn: $name` — PITR replay to the captured LSN
	//     (the linkage is a side-effect of the $-ref).
	//   * `name: <name>`  — explicit linkage with no PITR
	//     replay, for scenarios where capture is taken before
	//     take_backup AND no later DML happens (so the
	//     restored cluster's natural end-of-backup state has
	//     the same checksum as the captured live state).
	//
	// The PITR-with-`$name` shape needs the WAL covering the
	// captured LSN to be present.  IncludeWAL=true on the
	// basebackup (testkit default) carries [backup-start,
	// backup-end] inline, so a captured LSN inside that range
	// replays cleanly; a captured LSN AFTER backup-end requires
	// a wal_stream sidecar.  Scenarios that capture pre-backup
	// can stick with `name:` and skip --to-lsn entirely.
	var capRefName string
	// PITR action default: promote.  The CLI's standalone default
	// is `pause` — safest for an operator who wants to inspect a
	// half-replayed cluster before committing — but every testkit
	// scenario follows PITR with `assert_restored_match`, which
	// polls `pg_is_in_recovery() = 'f'` and times out at 180 s if
	// PG is paused.  Promoting at end-of-recovery is what the test
	// harness actually wants.  Reproduced 2026-05-12 in
	// L3_wal_stream_ddl_storm: PG correctly paused at
	// `recovery_target_lsn`, the assertion polled for 180 s, the
	// scenario logged "sandbox did not accept connections within
	// 180s" indistinguishably from the real recovery-loop bug
	// fixed in walfetchcmd.
	pitr := st.To != "" || st.ToLSN != "" || st.ToCheckpoint != ""
	if pitr {
		args = append(args, "--to-action", "promote")
	}
	if st.ToLSN != "" {
		resolved, err := resolveToLSN(st.ToLSN, state)
		if err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: "restore: " + err.Error()}
		}
		args = append(args, "--to-lsn", resolved)
		if strings.HasPrefix(st.ToLSN, "$") {
			capRefName = strings.TrimPrefix(st.ToLSN, "$")
		}
	} else if st.Name != "" {
		capRefName = st.Name
	}
	// --tablespace-mapping is repeatable on the CLI; each scenario
	// entry becomes one repetition.  Placeholder substitution lets
	// a scenario use $ARTEFACT_DIR to land remapped tablespaces in
	// its per-scenario temp tree without baking host paths into the
	// YAML.
	for _, m := range st.TablespaceMappings {
		resolved, err := substitutePlaceholders(m, state)
		if err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("restore: tablespace_mapping %q: %v", m, err)}
		}
		args = append(args, "--tablespace-mapping", resolved)
	}
	cmd := state.agentCmd(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Combined output for the diagnostic — both streams are
		// useful when a restore fails (CLI emits an error JSON
		// block on stdout AND postgres-validation chatter on
		// stderr).
		combined := append(stdout.Bytes(), stderr.Bytes()...)
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("restore: %v (output: %s)", err, truncate(combined, 384))}
	}

	// pg_verifybackup post-restore: a free correctness check
	// against the manifest the agent wrote at backup time.
	// Soft-fail on missing binary so scenarios still pass on a
	// minimal CI host without postgresql-client; we surface the
	// skip as a separate event so an operator looking at the
	// run can see the gap.
	if vbBin, err := exec.LookPath("pg_verifybackup"); err == nil {
		vbCmd := exec.CommandContext(ctx, vbBin, target)
		var vbOut bytes.Buffer
		vbCmd.Stdout = &vbOut
		vbCmd.Stderr = &vbOut
		if err := vbCmd.Run(); err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("pg_verifybackup %s: %v (output: %s)",
					target, err, truncate(vbOut.Bytes(), 384))}
		}
		emit(out, "step.restore.verifybackup_ok", map[string]any{"target": target})
	} else {
		emit(out, "step.restore.verifybackup_skipped", map[string]any{
			"reason": "pg_verifybackup not on PATH (install postgresql-client to enable)",
		})
	}

	if capRefName != "" {
		// Stash the target so assert_restored_match (and any
		// future per-name post-restore step) can find this
		// datadir without re-deriving the path convention.
		if state.capturedStates != nil {
			if cs, ok := state.capturedStates[capRefName]; ok {
				cs.RestoredTarget = target
			}
		}
	}

	state.lastRestoreTarget = target
	emit(out, "step.restore.completed", map[string]any{
		"index":     idx,
		"backup_id": source,
		"target":    target,
	})
	return StepResult{Index: idx, Kind: st.Kind, Pass: true,
		Message: fmt.Sprintf("restored %s to %s", source, target)}
}

// runInject dispatches into the inject fault registry against the
// topology's targets.  Two input shapes:
//
//   - `action: "signal(target=pg_random, sig=15)"` — raw
//     registry-format string; passed straight through.  Operators
//     who need a primitive the convenience map below doesn't cover
//     drop down to this form.
//   - `kind: <name>` plus optional `target:` / `signal:` —
//     convenience form mapped to a raw action by buildAction.
//
// After Apply returns the runner waits HealWindow (if set) before
// firing the recovery callback.  For PG-killing signal faults the
// wait is a *max-budget* poll for `select 1` — see waitPGReady.
// For all other faults (agent signals, cgroup_squeeze, pause_archive)
// the wait stays a fixed sleep so the fault has time to "soak"
// before recovery.  The scenario YAML is responsible for asserting
// whatever invariant survives the fault.
func runInject(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	if state.targets == nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "inject: topology produced no targets — provider exposes no fault surface"}
	}
	action, err := buildAction(st)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "inject: " + err.Error()}
	}
	emit(out, "step.inject.applying", map[string]any{
		"index":  idx,
		"action": action,
	})
	recovery, err := inject.DefaultRegistry.Apply(ctx, action, state.targets)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("inject %q: %v", action, err)}
	}
	if st.HealWindow != "" {
		d, perr := time.ParseDuration(st.HealWindow)
		if perr != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("inject: parse heal_window %q: %v", st.HealWindow, perr)}
		}
		if actionWantsPGReadyPoll(action) && state.currentDSN() != "" {
			// PG-killing signal fault — heal_window is a *max-budget*
			// for PG-ready, not a fixed sleep.  Returns as soon as PG
			// accepts connections so we don't burn the rest of the
			// budget on a quiet host.  Surfaces "PG didn't come back"
			// as a clear inject error rather than letting the next
			// step's connect-refused be the surprise.
			//
			// Docker treats `docker kill` (any signal) as user-stop,
			// so neither `--restart=always` nor `unless-stopped` brings
			// PG back after our signal.  We pass the PG docker targets
			// to waitPGReady so it can `docker start` them while
			// polling — idempotent on running containers, the actual
			// restart on exited ones.
			var pgTargets []*inject.DockerTarget
			if state.targets != nil {
				if pgs, perr := state.targets.Pick("pg"); perr == nil {
					for _, t := range pgs {
						if dt, ok := t.(*inject.DockerTarget); ok {
							pgTargets = append(pgTargets, dt)
						}
					}
				}
			}
			startWait := time.Now()
			if werr := waitPGReady(ctx, state.currentDSN, d, pgTargets); werr != nil {
				return StepResult{Index: idx, Kind: st.Kind, Pass: false,
					Message: fmt.Sprintf("inject: PG did not become ready within heal_window %s after %s: %v",
						d, action, werr)}
			}
			emit(out, "step.inject.pg_ready", map[string]any{
				"index":      idx,
				"action":     action,
				"elapsed_ms": time.Since(startWait).Milliseconds(),
				"budget":     d.String(),
			})
		} else {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		}
	}
	if recovery != nil {
		if rerr := recovery(ctx); rerr != nil {
			emit(out, "step.inject.recovery_failed", map[string]any{
				"index": idx, "error": rerr.Error(),
			})
		}
	}
	emit(out, "step.inject.completed", map[string]any{
		"index": idx, "action": action,
	})
	return StepResult{Index: idx, Kind: st.Kind, Pass: true, Message: "applied " + action}
}

// actionWantsPGReadyPoll returns true for inject actions whose recovery
// is "PG comes back": signal faults targeting pg / pg_random /
// leader / replica.  Other actions (agent signals, cgroup_squeeze,
// pause_archive, toxiproxy, sql) either leave PG up or have no
// PG-readiness primitive — for those heal_window stays a fixed
// sleep so the fault has time to "soak" before recovery.
func actionWantsPGReadyPoll(action string) bool {
	pa, err := inject.ParseAction(action)
	if err != nil {
		return false
	}
	if pa.Prefix != "signal" {
		return false
	}
	switch pa.Args["target"] {
	case "pg", "pg_random", "leader", "replica":
		return true
	}
	return false
}

// waitPGReady polls dsn for a successful `select 1` every second,
// up to budget.  Used by inject steps whose recovery is "PG comes
// back" (signal faults targeting PG) — we advance as soon as PG
// accepts connections rather than burning a fixed sleep.
//
// Why this matters in practice: under sustained host load (multiple
// concurrent campaigns hammering the docker daemon) PG's
// shutdown→restart-policy→accept-conns cycle has been observed
// taking 60-90s.  On a quiet host the same cycle is <10s.  The
// previous fixed-sleep heal_window had to be tuned to the slowest
// observed case, wasting time on quiet hosts and STILL silently
// expiring before PG was ready on the slowest ones — surfacing
// as `storage.unreachable` on the next take_backup.
//
// ensureUp is the list of docker containers that should be running
// for PG to come back.  Each tick calls `docker start` on each —
// docker treats `docker kill` as user-stop and won't auto-restart
// even with `--restart=always`, so we have to bring it back
// ourselves.  `docker start` on a running container is a harmless
// success.
//
// dsnFn is called every loop iteration.  testcontainers' default
// `0:5432` PortBinding re-randomises the host port across docker
// stop+start, so a DSN cached at the top of the heal-window block
// will point at a stale port after the restart.  Re-resolving on
// each tick picks up the new port.
//
// On budget exhaustion returns the last connection error so the
// caller can surface a useful root cause.
func waitPGReady(ctx context.Context, dsnFn func() string, budget time.Duration, ensureUp []*inject.DockerTarget) error {
	deadline := time.Now().Add(budget)
	var lastErr error
	for {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		// Best-effort restart of any container the caller passed.
		// 2s cap so a wedged docker daemon doesn't hijack the loop.
		for _, dt := range ensureUp {
			sctx, scancel := context.WithTimeout(ctx, 2*time.Second)
			_ = dt.Start(sctx)
			scancel()
		}
		dsn := dsnFn()
		// New *sql.DB per attempt — pool state isn't useful across
		// a PG bounce, and Open is cheap.  2s connect+ping cap
		// keeps the loop responsive while PG is still down.
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		db, oerr := sql.Open("pgx", dsn)
		if oerr != nil {
			lastErr = oerr
		} else {
			if perr := db.PingContext(cctx); perr == nil {
				_ = db.Close()
				cancel()
				return nil
			} else {
				lastErr = perr
				_ = db.Close()
			}
		}
		cancel()
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = fmt.Errorf("budget %s exhausted with no recorded error", budget)
			}
			return lastErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// buildAction translates a scenario step's inject fields into the
// raw inject-registry action string.  Precedence: explicit Action
// wins; otherwise we map InjectKind + the convenience fields to a
// known primitive.  Unknown shapes return a typed error so the
// scenario fails fast with a pointer to the primitives we know.
//
// Mappings (extend cautiously — every new convenience kind is a
// surface area the YAML schema commits to):
//
//	kind=agent_kill,  signal=N      → signal(target=agent_random, sig=N)
//	kind=pg_kill,     signal=N      → signal(target=pg_random, sig=N)
//	kind=patroni_failover, target=T → patroni_switchover(target=T)
//	kind=disk_full,   target=T      → disk_full(target=T, fill=98%)
//	kind=pause_archive              → pause_archive(target=agent)
//
// `target:` overrides the default target for the mapped primitive.
// `signal:` overrides the default signal where one applies.
func buildAction(st scenario.Step) (string, error) {
	if st.Action != "" {
		return st.Action, nil
	}
	switch st.InjectKind {
	case "":
		return "", fmt.Errorf("either `action:` or `kind:` is required")
	case "agent_kill":
		t := st.Target
		if t == "" {
			t = "agent_random"
		}
		sig := st.Signal
		if sig == 0 {
			sig = 15
		}
		return fmt.Sprintf("signal(target=%s, sig=%d)", t, sig), nil
	case "pg_kill":
		t := st.Target
		if t == "" {
			t = "pg_random"
		}
		sig := st.Signal
		if sig == 0 {
			sig = 9
		}
		return fmt.Sprintf("signal(target=%s, sig=%d)", t, sig), nil
	case "patroni_failover", "patroni_switchover":
		t := st.Target
		if t == "" {
			t = "patroni"
		}
		return fmt.Sprintf("patroni_switchover(target=%s)", t), nil
	case "disk_full":
		t := st.Target
		if t == "" {
			t = "repo"
		}
		return fmt.Sprintf("disk_full(target=%s, fill=98%%)", t), nil
	case "pause_archive":
		t := st.Target
		if t == "" {
			t = "agent"
		}
		return fmt.Sprintf("pause_archive(target=%s)", t), nil
	case "docker_pause":
		// Freeze a docker container to simulate a network-storm /
		// GC-pause class outage.  The canonical use is target=sink:
		// the storage emulator stops processing IO for the heal
		// window, exercising the backup-tool's retry budget.
		t := st.Target
		if t == "" {
			t = "sink"
		}
		return fmt.Sprintf("docker_pause(target=%s)", t), nil
	}
	return "", fmt.Errorf("unknown inject kind %q (known: agent_kill, pg_kill, patroni_failover, disk_full, pause_archive, docker_pause — or set `action:` for the raw registry form)", st.InjectKind)
}

// runLoad re-applies the scenario's load file in a loop for the
// step's Duration.  Used to drive continuous load between
// faults — e.g. between Patroni switchovers in a failover-loop
// scenario.
//
// Why a loop and not a single re-application: the load file
// captures one "round" of workload (DDL + an insert burst); for
// failover testing we want sustained writes that span multiple
// leader changes.  Looping the whole file picks up the
// data-modification ops on every pass while DDL ops are safely
// no-op via IF NOT EXISTS.
//
// All in-tree ops are re-runnable:
//
//	create_table   — CREATE TABLE IF NOT EXISTS
//	create_index   — CREATE INDEX IF NOT EXISTS
//	insert_rows    — deterministic PRNG; rows aren't UNIQUE so
//	                 repeats just append more data
//	vacuum         — idempotent
//	checkpoint     — observability event, not a SQL command
//
// Empty Duration (or "0") collapses to a single re-application,
// matching the legacy behaviour an operator might expect from
// `run_load: {}` with no duration.
// runSQLStep executes a single raw SQL statement against the
// current primary, in autocommit.  It is the escape hatch for a
// DDL/DML the load-file op vocabulary doesn't cover (VACUUM FULL,
// an ad-hoc CREATE TABLE) AT A PRECISE POINT between steps — the
// scenario `load:` file cannot serve this because the runner
// force-applies it once at scenario start, before any step.
//
// The DSN is re-resolved on entry (same reason run_load does it:
// an earlier inject may have bounced PG to a new host port or
// promoted a different node), so the statement always lands on
// the writable primary.
func runSQLStep(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	if st.Statement == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "sql: `statement:` is required"}
	}
	dsn := state.currentDSN()
	if dsn == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "sql: topology returned empty DSN (no leader reachable)"}
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("sql: open: %v", err)}
	}
	defer func() { _ = db.Close() }()
	emit(out, "step.sql.executing", map[string]any{"index": idx})
	if _, err := db.ExecContext(ctx, st.Statement); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("sql: %v", err)}
	}
	return StepResult{Index: idx, Kind: st.Kind, Pass: true, Message: "sql ok"}
}

func runLoad(ctx context.Context, _ *sql.DB, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	if state.loadFile == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "run_load step requires the scenario to declare `load: { file: ... }`"}
	}
	dur := time.Duration(0)
	if st.Duration != "" {
		d, err := time.ParseDuration(st.Duration)
		if err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("run_load: parse duration %q: %v", st.Duration, err)}
		}
		dur = d
	}
	deadline := time.Time{}
	if dur > 0 {
		deadline = time.Now().Add(dur)
	}
	emit(out, "step.run_load.starting", map[string]any{
		"index":    idx,
		"duration": st.Duration,
		"file":     state.loadFile,
	})
	passes := 0
	for {
		// Re-resolve the DSN on every pass.  After a Patroni
		// switchover the previous leader has been demoted to a
		// replica (read-only), so reusing the runner's
		// scenario-start *sql.DB would fail with
		//   ERROR: cannot execute CREATE TABLE in a read-only
		//          transaction (SQLSTATE 25006)
		// on the first DDL of the looped load file.  ConnString
		// polls /leader on every call so we always reach the
		// current writable primary.
		dsn := state.currentDSN()
		if dsn == "" {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("run_load pass %d: topology returned empty DSN (no leader reachable)", passes+1)}
		}
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("run_load pass %d: open: %v", passes+1, err)}
		}
		err = applyLoad(ctx, db, state.loadFile, out)
		_ = db.Close()
		if err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("run_load pass %d: %v", passes+1, err)}
		}
		passes++
		if dur == 0 || time.Now().After(deadline) {
			break
		}
		if err := ctx.Err(); err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("run_load: %v after %d passes", err, passes)}
		}
	}
	return StepResult{Index: idx, Kind: st.Kind, Pass: true,
		Message: fmt.Sprintf("ran load %d pass(es) over %s", passes, st.Duration)}
}

// agentCmd wraps exec.CommandContext for invocations of the
// agent binary, merging runState.agentEnv into the host
// environment.  Used by every step that shells out to
// `pg_hardstorage` so sink-supplied credentials (S3 keys,
// Azure SAS, etc.) reach the SDK's credential chain.
//
// When agentEnv is empty (file:// repo), the command runs
// with the host environment unchanged, matching pre-Sink
// behaviour exactly.
func (s *runState) agentCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, s.agentBin, args...)
	if len(s.agentEnv) > 0 {
		env := append([]string(nil), os.Environ()...)
		for k, v := range s.agentEnv {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	return cmd
}

// initRepo runs `pg_hardstorage repo init` against the
// scenario-scoped repo URL.  Idempotent on the agent side: if the
// repo already exists at that URL, init reports it cleanly.  We
// scope the repo to artefactDir so two scenarios running in
// parallel don't write to the same backing store.
func initRepo(ctx context.Context, state *runState, out io.Writer) error {
	cmd := state.agentCmd(ctx, "repo", "init", state.repoURL)
	combined, err := cmd.CombinedOutput()
	if err != nil {
		// Include the agent's own output so the scenario log
		// captures why init failed — typically a permission or
		// already-exists issue the operator can act on.
		return fmt.Errorf("repo init %s: %w (output: %s)",
			state.repoURL, err, truncate(combined, 256))
	}
	emit(out, "step.repo.init", map[string]any{
		"repo": state.repoURL,
	})
	return nil
}

// truncate caps a byte slice to n bytes for log lines.  Mirrors
// the testkit's other diagnostic-truncation helpers.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// formatTargetSummary is a tiny helper used by inject-step
// diagnostic events: how many targets the topology offers, by
// role.  Not load-bearing for the dispatch — purely log surface.
func formatTargetSummary(ts []inject.Target) string {
	if len(ts) == 0 {
		return "none"
	}
	byRole := map[string]int{}
	for _, t := range ts {
		byRole[t.Role()]++
	}
	parts := make([]string, 0, len(byRole))
	for role, n := range byRole {
		parts = append(parts, fmt.Sprintf("%s=%d", role, n))
	}
	return strings.Join(parts, ",") + " (total=" + strconv.Itoa(len(ts)) + ")"
}

// pickFreePort asks the kernel for an unused localhost port.
// Used by startRestoredSandbox so concurrent assert_restored_match
// containers don't collide on a fixed 5432 (with --network=host
// this would race the bind() call between sandboxes).
//
// Brief race window between Close() and the docker container's
// bind is acceptable for a per-test sandbox; the same shape is
// used by every internal/testkit/sink/* runtime.
func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
