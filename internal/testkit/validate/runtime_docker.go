// runtime_docker.go — docker-backed CellRuntime: drives a real compose stack for soak orchestrator.
package validate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/compose"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/report"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

// DockerCellRuntime is the production CellRuntime: it talks to
// a real PostgreSQL inside a docker-compose container, drives
// load via pgx, and shells out to pg_hardstorage for backups +
// restores.
//
// The runtime stays small on purpose — most of the work
// happens through three external surfaces (pgx / docker /
// pg_hardstorage CLI), and the schema picks the actual workload
// shape.  A future cellruntime that talks to a remote agent
// over its REST API can implement the same CellRuntime
// interface without touching the orchestrator.
type DockerCellRuntime struct {
	// Cell metadata derived from the fleet entry.  Set by
	// NewDockerCellRuntime.
	CellName   string
	Container  string // docker container name (lead container of the cell)
	HostPort   int    // host port mapping to the container's 5432
	Deployment string // pg_hardstorage deployment name (== fleet entry name)
	OS         string
	PGVersion  string
	Profile    config.Profile
	Schema     Schema
	Faults     *config.Faults

	// AgentBinary is the path to pg_hardstorage inside the
	// container.  Defaults to /usr/local/bin/pg_hardstorage.
	AgentBinary string

	// RepoURL is the --repo argument passed to backup +
	// restore.  Default file:///var/lib/pg_hardstorage/repo
	// (the path the testbed images bind-mount).
	RepoURL string

	// DockerBin is the docker / podman binary.  Default "docker".
	DockerBin string

	// PGUser is the libpq user.  Default "postgres".
	PGUser string

	// PGDatabase is the libpq database.  Default "postgres".
	PGDatabase string

	// Targets is the inject TargetSet for fault dispatch.
	// Constructed by NewDockerCellRuntime so the soak driver
	// doesn't have to plumb every container through.
	Targets inject.TargetSet

	// SinkKind is the per-cell storage backend to bring up
	// in Setup.  Empty keeps RepoURL = file:///var/lib/...
	// (the bind-mounted host repo-data dir, the legacy
	// behaviour).  Non-empty must match an entry in
	// internal/testkit/sink.SinkImages — Setup brings up
	// the matching emulator, sets RepoURL = sink.URL(), and
	// merges sink.EnvForAgent() into every dockerExec.
	SinkKind     string
	sinkRT       sink.Runtime
	sinkAgentEnv map[string]string

	// Internal state.
	conn        *pgx.Conn
	rng         *rand.Rand
	totalBytes  int64
	backupCount int

	// Sustained-load sidecar state.  Set when
	// StartSustainedLoad launches a backgrounded `docker
	// exec pgbench`; consumed by StopSustainedLoad which
	// kills the process and parses pgbench's final report
	// from the captured stdout/stderr.
	sustainedCmd       *exec.Cmd
	sustainedStdout    *bytes.Buffer
	sustainedStderr    *bytes.Buffer
	sustainedStartedAt time.Time
	sustainedCancel    context.CancelFunc
	sustainedDone      chan struct{}
	walPreCount        int64 // pg_stat_wal.wal_bytes at writer start; 0 = not sampled

	// WAL-stream sidecar state.  Same shape as the sustained
	// writer — backgrounded `docker exec pg_hardstorage wal
	// stream` whose lifecycle is bracketed by
	// StartWALStream / StopWALStream.
	walStreamCmd    *exec.Cmd
	walStreamStdout *bytes.Buffer
	walStreamStderr *bytes.Buffer
	walStreamCancel context.CancelFunc
	walStreamDone   chan struct{}
}

// NewDockerCellRuntime builds a DockerCellRuntime from a fleet
// entry + its allocated host port + a workload profile.  The
// runtime resolves its lead container name via the same logic
// compose generate uses, so the soak driver never re-derives.
//
// `project` is the docker-compose project name; it gets
// prefixed onto leadContainer to form the actual Docker
// container_name (matching what compose generate emits).  An
// empty project keeps the unprefixed legacy behaviour for
// stand-alone tests that don't run under compose.
func NewDockerCellRuntime(
	entry config.FleetEntry,
	project string,
	leadContainer string,
	hostPort int,
	profile config.Profile,
	faults *config.Faults,
	seed int64,
) (*DockerCellRuntime, error) {
	schema, err := LookupSchema(profile.Schema)
	if err != nil {
		return nil, err
	}
	return &DockerCellRuntime{
		CellName:    entry.Name,
		Container:   compose.PrefixedContainerName(project, leadContainer),
		HostPort:    hostPort,
		Deployment:  entry.Name,
		OS:          entry.OS,
		PGVersion:   entry.PG,
		Profile:     profile,
		Schema:      schema,
		Faults:      faults,
		AgentBinary: "/usr/local/bin/pg_hardstorage",
		// RepoURL defaults to the bind-mounted host repo-data
		// dir; if entry.Sink is set, Setup will overwrite it
		// with the sink runtime's URL once the emulator is
		// up.  Keeping the legacy default here means cells
		// without an explicit sink behave exactly as before.
		RepoURL:    "file:///var/lib/pg_hardstorage/repo",
		DockerBin:  "docker",
		PGUser:     "postgres",
		PGDatabase: "postgres",
		SinkKind:   entry.Sink,
		rng:        rand.New(rand.NewSource(seed)),
	}, nil
}

// Name implements CellRuntime.
func (d *DockerCellRuntime) Name() string { return d.CellName }

// Setup waits for PG to accept connections, opens a pgx
// connection, runs the schema's Setup, and constructs the
// inject TargetSet.
//
// The 90 s upper bound is generous on purpose: a cold first-
// boot of a freshly-built testbed image runs initdb (~5–15 s
// per cell on a small CI VM, more under disk contention from
// 5+ cells starting in parallel) before PG even listens.
// A fail-fast 30 s budget produced false positives on real
// laptops; 90 s still surfaces actual broken-listen issues
// quickly while letting slow distros (opensuse + libfaketime-
// fork-cost) reach a healthy state.
//
// On timeout the runtime captures `docker inspect` (running?
// exited? exit code?) and the last 200 lines of `docker logs`
// for the cell's lead container, and folds them into the
// returned error.  Without this the soak report says only
// "connection refused" — useless for diagnosing the fast-failing
// reasons (entrypoint script crashed, PG refused to bind,
// initdb ran out of disk) the operator actually needs to see.
func (d *DockerCellRuntime) Setup(ctx context.Context) error {
	// Per-cell sink — must come before any agent invocation
	// since the repo URL depends on the sink being up.  When
	// SinkKind is empty we keep the legacy file:// repo
	// behaviour entirely (no sink container, no env merge).
	if d.SinkKind != "" {
		s, err := sink.New(d.SinkKind)
		if err != nil {
			return fmt.Errorf("setup %s: sink: %w", d.CellName, err)
		}
		if err := s.Up(ctx); err != nil {
			return fmt.Errorf("setup %s: sink up: %w", d.CellName, err)
		}
		d.sinkRT = s
		d.RepoURL = s.URL()
		d.sinkAgentEnv = s.EnvForAgent()
	}

	dsn := d.dsn()
	conn, err := waitForPG(ctx, dsn, 90*time.Second)
	if err != nil {
		// Capture diagnostics before the orchestrator tears
		// the container down.  Best-effort: any tool failure
		// here is itself part of the error string.
		diag := d.diagnoseContainer(context.Background())
		return fmt.Errorf("setup %s: %w%s", d.CellName, err, diag)
	}
	d.conn = conn

	if err := d.Schema.Setup(d.exec(ctx)); err != nil {
		return fmt.Errorf("setup %s: schema: %w", d.CellName, err)
	}

	// Bootstrap the repo before any iteration tries to take a
	// backup.  Without this, the very first `backup` call
	// errors out with notfound.repo and every subsequent
	// iteration looks like a regression even though the cell
	// is healthy.  Idempotent: a stale file:// repo from a
	// previous run gets a `conflict.repo_exists` error which
	// we treat as success.
	if err := d.ensureRepoInit(ctx); err != nil {
		return fmt.Errorf("setup %s: repo init: %w", d.CellName, err)
	}

	// Build the inject TargetSet — the lead container as
	// "agent" and "pg" (same target serves both roles in v1
	// since the agent runs inside the PG container).  A
	// "repo" target is the same container until we wire a
	// separate repo container.
	targets := []inject.Target{
		&inject.DockerTarget{Container: d.Container, RoleStr: "agent", DockerBin: d.DockerBin},
		&inject.DockerTarget{Container: d.Container, RoleStr: "pg", DockerBin: d.DockerBin},
		&inject.DockerTarget{Container: d.Container, RoleStr: "repo", DockerBin: d.DockerBin},
	}
	// Toxiproxy front door: container name + "-tox" by
	// convention from compose generate.
	targets = append(targets, &inject.DockerTarget{
		Container: d.Container + "-tox", RoleStr: "toxiproxy", DockerBin: d.DockerBin,
	})
	// Per-cell sink (when configured): register the sink's
	// container under role "sink" so inject primitives like
	// `sink_pause` / `sink_unpause` can find it.  This is
	// what lets a scenario step simulate "S3 unreachable
	// for 30s" — pause the sink, do work, unpause.
	if d.sinkRT != nil && d.sinkRT.ContainerName() != "" {
		targets = append(targets, &inject.DockerTarget{
			Container: d.sinkRT.ContainerName(),
			RoleStr:   "sink",
			DockerBin: d.DockerBin,
		})
	}
	d.Targets = inject.NewStaticTargetSet(targets, time.Now().UnixNano())
	return nil
}

// DriveLoad runs one schema iteration.
func (d *DockerCellRuntime) DriveLoad(ctx context.Context) (int64, error) {
	// A fault since the last iteration (signal, cgroup_squeeze,
	// toxiproxy, ...) may have killed PostgreSQL and closed the
	// long-lived load connection.  Without a reconnect the very
	// first such fault wedges the load driver for the entire
	// soak — every later iteration fails with
	// "failed to deallocate cached statement(s): conn closed"
	// and the "sustained load" stops being sustained.  Reopen a
	// dead connection before driving.
	if err := d.ensureLoadConn(ctx); err != nil {
		return 0, err
	}
	bytes, err := d.Schema.Iteration(d.exec(ctx), d.rng, d.Profile.ChurnMBPerMin)
	d.totalBytes += bytes
	if err != nil && d.reindexCorruptIndex(ctx, err) {
		// A torn_page fault that lands on a user index metapage
		// leaves that index "is not a btree" (SQLSTATE XX002), and
		// every later INSERT/UPDATE the schema drives keeps hitting
		// it — wedging the load driver for the rest of the soak
		// exactly like the conn-closed case ensureLoadConn heals
		// above (an 8h run lost ~234 iterations on a single cell to
		// this).  reindexCorruptIndex rebuilt the index from the
		// heap, so the next tick drives load again.  We still return
		// this tick's error so the soak records the fault's bite —
		// the heal only stops it from being permanent.
		return bytes, fmt.Errorf("%w (reindexed corrupt index to un-wedge load)", err)
	}
	return bytes, err
}

// corruptIndexRE pulls the index name out of a PG XX002
// `index "<name>" is not a btree` error so reindexCorruptIndex
// can rebuild exactly that index.
var corruptIndexRE = regexp.MustCompile(`index "([^"]+)" is not a btree`)

// corruptIndexName returns the index name from the XX002
// `index "<name>" is not a btree` error a torn_page fault leaves
// when it tears an index metapage, or "" when err is anything
// else.  A torn heap page (or any non-index corruption) surfaces
// a different error, so it returns "" and the caller leaves it
// alone.  Kept pure (no conn) so it is unit-testable.
func corruptIndexName(err error) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "XX002" {
		return ""
	}
	if m := corruptIndexRE.FindStringSubmatch(pgErr.Message); m != nil {
		return m[1]
	}
	return ""
}

// reindexCorruptIndex REINDEXes the index named in a torn-metapage
// XX002 error so it is rebuilt from the heap (the torn metapage
// then no longer matters).  Returns true only when it issued a
// REINDEX that succeeded.
func (d *DockerCellRuntime) reindexCorruptIndex(ctx context.Context, err error) bool {
	name := corruptIndexName(err)
	if name == "" || d.conn == nil {
		return false
	}
	// pgx.Identifier.Sanitize quotes the name, so an index
	// called e.g. `weird"name` can't break the statement.
	stmt := "REINDEX INDEX " + pgx.Identifier{name}.Sanitize()
	_, rerr := d.conn.Exec(ctx, stmt)
	return rerr == nil
}

// ensureLoadConn reopens d.conn when it has been closed — e.g.
// by a fault that killed PostgreSQL.  A live connection is left
// untouched; a nil or closed one is replaced with a fresh one,
// waiting up to 30s for PG to come back (a fault may still be
// inside its heal window).
func (d *DockerCellRuntime) ensureLoadConn(ctx context.Context) error {
	if d.conn != nil && !d.conn.IsClosed() {
		return nil
	}
	if d.conn != nil {
		_ = d.conn.Close(ctx)
		d.conn = nil
	}
	conn, err := waitForPG(ctx, d.dsn(), 30*time.Second)
	if err != nil {
		return fmt.Errorf("reconnect load connection: %w", err)
	}
	d.conn = conn
	return nil
}

// Seed bulk-loads the cell's PG to roughly sizeGB of on-disk
// data via `pgbench -i -s <scale>`.  Scale is sized at 67 per
// GB (pgbench scale=1 produces ~15 MB after PG overhead +
// indexes).  The pgbench binary is located inside the
// container by walking the standard install paths — same
// logic the testbed entrypoint uses, so it works across both
// pgdg-apt (/usr/lib/postgresql/N/bin) and distro packaging
// (/usr/pgsql-N/bin or /usr/bin).
//
// The Profile.SeedTargetGB field takes precedence over the
// argument — the orchestrator calls Seed unconditionally and
// the cell decides based on its own profile.  Argument is
// honoured only when the profile leaves the field zero
// (useful for tests that drive Seed directly).  Either way,
// a non-positive resolved size is a no-op.
func (d *DockerCellRuntime) Seed(ctx context.Context, sizeGB int) error {
	target := d.Profile.SeedTargetGB
	if target <= 0 {
		target = sizeGB
	}
	if target <= 0 {
		return nil
	}
	scale := target * 67

	// Resolve pgbench's path inside the container so we don't
	// depend on whichever pg version's wrappers happen to be
	// on PATH.  Fail loud if absent — a missing pgbench means
	// the testbed image was built without postgresql-client,
	// which is a build regression worth surfacing.
	pgBinDir, err := d.locateContainerPGBin(ctx, "pgbench")
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}

	out, err := d.dockerExec(ctx,
		"sudo", "-u", d.PGUser,
		pgBinDir+"/pgbench", "-i", "-s", fmt.Sprintf("%d", scale),
		"-d", d.PGDatabase)
	if err != nil {
		return fmt.Errorf("seed %s: pgbench -i -s %d: %w (output: %s)",
			d.CellName, scale, err, truncate(out, 512))
	}
	return nil
}

// locateContainerPGBin returns the directory containing the
// requested PG binary inside the cell's lead container, by
// walking the same set of install paths as
// dockerfiles/testbed/entrypoint-pg.sh.  Returns an error if
// the binary isn't present in any of them.
func (d *DockerCellRuntime) locateContainerPGBin(ctx context.Context, binName string) (string, error) {
	out, err := d.dockerExec(ctx, "bash", "-c",
		`for d in /usr/lib/postgresql/*/bin /usr/pgsql-*/bin /usr/bin; do `+
			`if [ -x "$d/`+binName+`" ]; then echo "$d"; exit 0; fi; `+
			`done; exit 1`)
	if err != nil {
		return "", fmt.Errorf("locate %s in %s: %w (output: %s)",
			binName, d.Container, err, truncate(out, 200))
	}
	return strings.TrimSpace(string(out)), nil
}

// ErrCellNotReady signals that the lead container isn't running
// when a backup is dispatched — typically because a fault stopped
// it and recovery hasn't completed.  The orchestrator treats this
// as a soft skip rather than a backup failure of the system under
// test (see runCellLoop).
var ErrCellNotReady = errors.New("cell not ready: lead container not running")

// cellDownDockerErr reports whether a docker-exec error is the
// daemon saying the target container is not in a usable state —
// stopped or removed by a fault.  These phrases originate only
// from dockerd describing container state, so matching them
// cannot mask a genuine pg_hardstorage / pg_verifybackup error.
// Used alongside containerRunning to catch the race where a fault
// stops the cell *during* an exec (the exec fails, but the
// container may already be back up by the time we re-inspect).
func cellDownDockerErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "is not running") ||
		strings.Contains(s, "No such container") ||
		strings.Contains(s, "no such container")
}

// containerRunning returns true iff `docker inspect` reports
// State.Running == true for the lead container.  Any inspect
// failure (missing container, daemon error) returns false so the
// caller treats it like a stopped cell.
func (d *DockerCellRuntime) containerRunning(ctx context.Context) bool {
	if d.Container == "" {
		return true
	}
	out, err := exec.CommandContext(ctx, d.dockerBin(),
		"inspect", "--format", "{{.State.Running}}",
		d.Container).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// TakeBackup invokes `pg_hardstorage backup` inside the
// container.
func (d *DockerCellRuntime) TakeBackup(ctx context.Context) (string, error) {
	// Gate on container state — a fault may have just stopped
	// it and recovery hasn't completed.  Without this gate the
	// docker exec below fails with "container ... is not
	// running", which gets recorded as a backup failure even
	// though the system under test is fine; the runner just
	// dispatched the backup too early.
	if !d.containerRunning(ctx) {
		return "", ErrCellNotReady
	}
	d.backupCount++
	// Use dockerExecCapture (NOT dockerExec) so stderr noise from
	// the agent or PG (NOTICE, progress events, fault chatter)
	// does not get mixed into the JSON we're about to parse.
	// The earlier all-combined dockerExec masked this for the
	// happy path but failed every time a fault was active during
	// the backup, falling back to a synthesised backup ID and
	// breaking the verify-restore step.
	// `--stall-timeout 5m` so an IO-starved backup (host disk
	// saturated by 4-slot soak parallelism) fails cleanly with
	// backup.io_starved instead of wedging the orchestrator
	// indefinitely.  See pg_hardstorage backup --help for the
	// flag's semantics; 5m is the recommended default for
	// "no-progress means we're broken, not just slow."
	// --include-wal bundles every WAL segment between start_lsn
	// and stop_lsn into the basebackup tar stream (equivalent to
	// pg_basebackup -X stream).  Production deployments leave
	// this off and rely on `wal stream` to archive the segments
	// continuously, but the soak runs verify shortly after
	// backup and there's no guarantee the streamer has caught up
	// to the backup's stop_lsn by then.  Without --include-wal,
	// every verify whose stop_lsn lands past the streamer's
	// confirmed_flush hangs in postverify's pg_ctl start
	// (recovery waiting on a segment restore_command can't find
	// in the repo yet) until pg_ctl's 180 s timeout.  Including
	// WAL in the basebackup itself makes the verify
	// self-contained: pg_wal/ has everything redo needs to reach
	// stop_lsn, recovery_target=immediate fires, pg_ctl exits.
	//
	// --stall-timeout 5m so an IO-starved backup (host disk
	// saturated by 4-slot soak parallelism) fails cleanly with
	// backup.io_starved instead of wedging the orchestrator
	// indefinitely.
	// Transient-failure retry: a fault recently applied
	// (pg_kill_9, agent_kill_15, disk_full) may leave PG mid-
	// recovery when the backup step fires.  The agent's connect
	// fails with `storage.unreachable: cannot connect to
	// PostgreSQL: ... connection refused`, which is a transient
	// signal — not a real "backup broken" outcome.  The user
	// wants 100% reliability, so we retry with exponential
	// backoff (1 s, 2 s, 4 s) before giving up.  Three retries
	// covers the worst-observed "PG restarted but isn't ready"
	// window from soak testing (~5 s after a SIGKILL on a busy
	// cell) with comfortable margin.  Pure transient handler:
	// any error that isn't "PG unreachable" propagates on the
	// first try.
	var (
		stdout, stderr []byte
		err            error
	)
	for attempt, backoff := 0, time.Second; attempt < 3; attempt++ {
		stdout, stderr, err = d.dockerExecCapture(ctx,
			d.AgentBinary, "backup", d.Deployment,
			"--pg-connection", d.containerDSN(),
			"--repo", d.RepoURL,
			"--include-wal",
			"--stall-timeout", "5m",
			"-o", "json")
		if err == nil {
			break
		}
		if !isPGUnreachable(stdout, stderr) {
			break
		}
		// Wait, then re-poll the container; if the cell isn't
		// running we surface the not-ready signal to the caller
		// just like the pre-check above.
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if !d.containerRunning(ctx) {
			return "", ErrCellNotReady
		}
	}
	if err != nil {
		// Diagnostic display still wants combined output —
		// stderr is where the operator-relevant context
		// usually lives on a failure.
		combined := make([]byte, 0, len(stdout)+len(stderr))
		combined = append(combined, stdout...)
		combined = append(combined, stderr...)
		return "", fmt.Errorf("backup %s: %w (output: %s)",
			d.CellName, err, truncate(combined, 256))
	}
	// Parse the backup ID from the agent's stdout-only JSON output.
	// Schema: `{"result": {"backup_id": "..."}}`.  encoding/json
	// (stdlib, no dependency cost) over a narrow struct keeps the
	// match robust to whitespace, key ordering, and incidental
	// fields the agent might add later.
	var parsed struct {
		Result struct {
			BackupID string `json:"backup_id"`
		} `json:"result"`
	}
	id := ""
	if jerr := json.Unmarshal(stdout, &parsed); jerr == nil {
		id = parsed.Result.BackupID
	}
	if id == "" {
		// Fall back to a synthesised name based on the
		// backup count — the verify step will look it up.
		// With stdout/stderr split, this should now only
		// fire if the agent truly didn't emit a backup_id
		// (e.g. it crashed after exit 0, which shouldn't
		// happen, or the JSON shape changed).
		id = fmt.Sprintf("%s.full.%04d", d.Deployment, d.backupCount)
	}
	return id, nil
}

// VerifyRestore restores backupID into a sandbox path inside the
// container and lets `pg_hardstorage restore`'s own gates verify
// it — L2 (in-process pg_verifybackup over the pristine restored
// files) and L3 (cluster-start smoke test).
func (d *DockerCellRuntime) VerifyRestore(ctx context.Context, backupID string) error {
	// Gate on container state, exactly as TakeBackup does — a
	// fault (e.g. `signal` killing the cell) may have stopped the
	// container and recovery hasn't completed.  Without this gate
	// the docker exec below fails with "container ... is not
	// running" and the orchestrator scores it as a verify failure
	// of the system under test, when it is really just a cell
	// mid-recovery.  ErrCellNotReady tells the orchestrator to
	// soft-skip this verify, like backup_skipped_cell_down.
	if !d.containerRunning(ctx) {
		return ErrCellNotReady
	}

	target := fmt.Sprintf("/tmp/restore-%s-%d", d.Deployment, time.Now().UnixNano())
	defer func() {
		_, _ = d.dockerExec(context.Background(),
			"rm", "-rf", target)
	}()

	// `pg_hardstorage restore` is itself the complete restore-verify
	// gate, so the testkit does NOT run a second pg_verifybackup:
	//
	//   - L2: an in-process pg_verifybackup over the freshly-written,
	//     PRISTINE restored files — hard-fails the restore on any
	//     byte-level mismatch (see internal/restore/restore.go).
	//   - L3 (--verify-restore required): a cluster-start smoke test
	//     that boots PostgreSQL on the restored data dir.
	//
	// The testkit previously ran its own external `pg_verifybackup`
	// here, but that fired AFTER L3 had booted PG — the data dir then
	// carries runtime files (pg_internal.init, pgstat.stat,
	// postmaster.pid/opts, the postverify log) that are absent from
	// the backup manifest, so pg_verifybackup always reported a
	// spurious "present on disk but not in the manifest" mismatch.
	// `--verify skip` suppresses the CLI's own post-boot external
	// pg_verifybackup for exactly the same reason; L2 already covers
	// byte-integrity, and on the pristine restore where it belongs.
	//
	// Omitting --to / --to-lsn / --to-name leaves PITR disarmed (the
	// CLI defaults to "no recovery target" when no --to* is set).
	out, err := d.dockerExec(ctx,
		d.AgentBinary, "restore", d.Deployment, backupID,
		"--repo", d.RepoURL,
		"--target", target,
		"--verify", "skip",
		"--verify-restore", "required")
	if err != nil {
		// A fault may have stopped the cell between the gate above
		// and this exec — re-check (and inspect the error text for
		// the kill-during-exec race) and soft-skip rather than
		// reporting a verify failure for a down cell.
		if !d.containerRunning(ctx) || cellDownDockerErr(err) {
			return ErrCellNotReady
		}
		// 4 KiB cap: pg_hardstorage's restore-failure JSON nests the
		// postverify pg_ctl output + postgresql.log tail; 256 B
		// truncated the actual reason mid-line.
		return fmt.Errorf("restore-verify %s/%s: %w (output: %s)",
			d.CellName, backupID, err, truncate(out, 4096))
	}
	return nil
}

// ApplyFault dispatches through the inject registry against the
// runtime's target set.
func (d *DockerCellRuntime) ApplyFault(ctx context.Context, action string) (inject.Recovery, error) {
	if d.Targets == nil {
		return nil, errors.New("ApplyFault: target set not initialised (Setup not called?)")
	}
	return inject.DefaultRegistry.Apply(ctx, action, d.Targets)
}

// Teardown closes the pgx connection and brings down the
// per-cell sink (if any).  Best-effort across both — a
// teardown error in one path doesn't block the other; we'd
// rather leak slightly than mask the original failure.
func (d *DockerCellRuntime) Teardown(ctx context.Context) error {
	if d.conn != nil {
		_ = d.conn.Close(ctx)
		d.conn = nil
	}
	if d.sinkRT != nil {
		_ = d.sinkRT.Down(ctx)
		d.sinkRT = nil
	}
	return nil
}

// SnapshotMetadataPaths returns the paths the reproducer should
// bundle if this cell hits a failure.  v1: just the agent's
// audit log + the repo's manifest dir.
func (d *DockerCellRuntime) SnapshotMetadataPaths() []string {
	return []string{
		fmt.Sprintf("/var/lib/pg_hardstorage/audit"),
		fmt.Sprintf("/var/lib/pg_hardstorage/repo/manifests"),
	}
}

// StartSustainedLoad runs `pgbench -c $clients -j $clients -T
// 100000 -R $rate --no-vacuum` inside the cell's container so
// the iteration loop's backups + faults run against an
// actively-modified database.  The TPC-B workload pgbench
// drives by default UPDATEs three indexed tables per
// transaction → maximum WAL pressure per row, the right
// pathology for testing a backup tool's behaviour under
// concurrent modification.
//
// The duration -T is set absurdly large; we rely on
// StopSustainedLoad sending SIGTERM at teardown rather than
// pgbench's own timer.  --no-vacuum skips the initial vacuum
// pgbench would otherwise do (we already seeded; another full
// vacuum would block the soak for minutes on a 10 GB DB).
//
// No-op when Profile.SustainedClients == 0.  Profile.PGUser /
// PGDatabase / Profile.SustainedRateTPS feed the pgbench argv.
func (d *DockerCellRuntime) StartSustainedLoad(ctx context.Context) error {
	if d.Profile.SustainedClients <= 0 {
		return nil
	}
	if d.sustainedCmd != nil {
		return errors.New("StartSustainedLoad: already running")
	}
	pgBin, err := d.locateContainerPGBin(ctx, "pgbench")
	if err != nil {
		return fmt.Errorf("sustained load: %w", err)
	}

	// Sample WAL counter so StopSustainedLoad can compute
	// "WAL bytes written during the writer's lifetime".  Best-
	// effort: pg_stat_wal exists since PG14; older clusters
	// just leave the counter at zero and the report shows "—".
	d.walPreCount = d.samplePGStatWAL(ctx)

	args := []string{
		pgBin + "/pgbench",
		"-c", fmt.Sprintf("%d", d.Profile.SustainedClients),
		"-j", fmt.Sprintf("%d", d.Profile.SustainedClients),
		"-T", "100000", // effectively forever; we kill on Stop
		"--no-vacuum",
		"-U", d.PGUser,
	}
	if d.Profile.SustainedRateTPS > 0 {
		args = append(args, "-R", fmt.Sprintf("%d", d.Profile.SustainedRateTPS))
	}
	args = append(args, d.PGDatabase)

	bgCtx, cancel := context.WithCancel(context.Background())
	full := append([]string{"exec", "-u", d.PGUser, d.Container}, args...)
	cmd := exec.CommandContext(bgCtx, d.dockerBin(), full...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("sustained load: pgbench start: %w", err)
	}

	d.sustainedCmd = cmd
	d.sustainedStdout = &stdout
	d.sustainedStderr = &stderr
	d.sustainedStartedAt = time.Now()
	d.sustainedCancel = cancel
	d.sustainedDone = make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(d.sustainedDone)
	}()
	return nil
}

// StopSustainedLoad terminates the pgbench writer started by
// StartSustainedLoad and parses its final report into
// LoadStats.  Returns nil/nil when no writer was running so
// the orchestrator can call Stop unconditionally.
func (d *DockerCellRuntime) StopSustainedLoad(ctx context.Context) (*report.LoadStats, error) {
	if d.sustainedCmd == nil {
		return nil, nil
	}
	// Cancel triggers SIGKILL on the docker-exec process,
	// which (because docker exec proxies signals) terminates
	// pgbench inside the container.  pgbench prints its
	// summary on TERM as well as on natural exit, so we
	// expect numbers in stdout regardless.
	d.sustainedCancel()
	select {
	case <-d.sustainedDone:
	case <-time.After(5 * time.Second):
		// pgbench should react within a second; longer means
		// docker is wedged.  Don't block teardown.
	}

	stats := &report.LoadStats{SustainedWriterRan: true}
	if d.sustainedStdout != nil {
		tps, p95 := parsePgbenchSummary(d.sustainedStdout.String())
		stats.TPSAvg = tps
		stats.LatencyP95Ms = p95
	}
	// WAL bytes written during the writer's lifetime.
	if post := d.samplePGStatWAL(ctx); post > 0 && d.walPreCount >= 0 {
		stats.WALBytesWritten = post - d.walPreCount
	}

	d.sustainedCmd = nil
	d.sustainedStdout = nil
	d.sustainedStderr = nil
	d.sustainedCancel = nil
	d.sustainedDone = nil
	return stats, nil
}

// StartWALStream runs `pg_hardstorage wal stream` inside the
// container, feeding committed WAL into the cell's repo
// concurrently with everything else the soak does.  This is
// what an enterprise deployment runs alongside a basebackup
// schedule; the soak's job is to prove it stays alive across
// faults and Patroni leader changes.
//
// Issue #31: previously a no-op when Profile.SustainedClients ==
// 0, on the (faulty) reasoning that an idle DB has no WAL to
// archive.  But every soak profile runs DriveLoad() each
// iteration (churn_mb_per_min > 0 even for oltp_smoke), and
// every iteration's TakeBackup commits a basebackup whose
// recovery NEEDS the trailing WAL segments in the repo —
// otherwise postverify hangs in "waiting for WAL to become
// available" until pg_ctl times out and the cell is recorded as
// restore.postverify_failed.  Make the streamer unconditional;
// running idle is cheap (it just sits waiting on the slot) and
// the whole point of the soak is to exercise the production
// archiving path.
func (d *DockerCellRuntime) StartWALStream(ctx context.Context) error {
	if d.walStreamCmd != nil {
		return errors.New("StartWALStream: already running")
	}
	bgCtx, cancel := context.WithCancel(context.Background())
	// `-u pgbackup` so the agent runs as the dedicated non-root
	// system user — the CLI gate at internal/cli/refuse_root.go
	// rejects euid 0 outright.  Matches production posture; the
	// testbed Dockerfile creates pgbackup at build time.
	full := []string{"exec", "-u", "pgbackup", d.Container,
		d.AgentBinary, "wal", "stream", d.Deployment,
		"--pg-connection", d.containerDSN(),
		"--repo", d.RepoURL,
	}
	cmd := exec.CommandContext(bgCtx, d.dockerBin(), full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("wal stream: start: %w", err)
	}

	d.walStreamCmd = cmd
	d.walStreamStdout = &stdout
	d.walStreamStderr = &stderr
	d.walStreamCancel = cancel
	d.walStreamDone = make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(d.walStreamDone)
	}()
	return nil
}

// StopWALStream terminates the streamer and computes the lag
// between the primary's current WAL position and the last
// segment the streamer flushed.  The lag is the headline
// number for "did continuous archiving keep up?"; an
// unbounded value here is the signal that the streamer fell
// behind the source under load.
func (d *DockerCellRuntime) StopWALStream(ctx context.Context) (*report.LoadStats, error) {
	if d.walStreamCmd == nil {
		return nil, nil
	}
	// Compute lag BEFORE killing the streamer — once it's
	// gone the "last flushed segment" query against the repo
	// would be racing the kill, and the answer would shift.
	stats := &report.LoadStats{WALStreamRan: true}
	if lag := d.computeWALStreamLag(ctx); lag >= 0 {
		stats.WALStreamLagBytes = lag
	}
	// Repo-side measurement: complements the live
	// pg_stat_replication query above by asking "what
	// actually committed to the backing store?".  Robust
	// to the in-between-iteration windows where
	// pg_stat_replication has no active row.
	if repoLag, segs := d.computeRepoSideWALLag(ctx); repoLag >= 0 {
		stats.WALRepoLagBytes = repoLag
		stats.WALSegmentsCommitted = segs
	}

	d.walStreamCancel()
	select {
	case <-d.walStreamDone:
	case <-time.After(5 * time.Second):
	}

	d.walStreamCmd = nil
	d.walStreamStdout = nil
	d.walStreamStderr = nil
	d.walStreamCancel = nil
	d.walStreamDone = nil
	return stats, nil
}

// samplePGStatWAL returns pg_stat_wal.wal_bytes for the cell
// or 0 if the view is unavailable (PG ≤ 13) or the query
// fails.  Used by Start/Stop SustainedLoad to compute "WAL
// bytes written during the writer's lifetime".
func (d *DockerCellRuntime) samplePGStatWAL(ctx context.Context) int64 {
	if d.conn == nil {
		return 0
	}
	var n int64
	row := d.conn.QueryRow(ctx, "select coalesce(wal_bytes, 0)::bigint from pg_stat_wal")
	if err := row.Scan(&n); err != nil {
		return 0
	}
	return n
}

// computeWALStreamLag returns the byte distance between the
// primary's current WAL position and the worst-lagging
// active streamer's flush_lsn.  -1 on any error reaching the
// primary; 0 means either "no consumer connected" or
// "consumer is fully caught up" — both render as "—" in the
// report, which is the right operator signal (a steady-zero
// is exactly what a healthy continuous archiver shows).
//
// Implementation note: pg_wal_lsn_diff returns numeric, not
// bigint, so we cast at the SQL layer.  Taking MAX across
// pg_stat_replication picks the worst-lagging consumer in
// case the deployment has multiple subscribers (replicas +
// pg_hardstorage wal stream both connect via the replication
// protocol).
func (d *DockerCellRuntime) computeWALStreamLag(ctx context.Context) int64 {
	if d.conn == nil {
		return -1
	}
	var lag int64
	err := d.conn.QueryRow(ctx, `
		SELECT COALESCE(
			MAX(pg_wal_lsn_diff(pg_current_wal_lsn(), flush_lsn)::bigint),
			0)
		FROM pg_stat_replication
		WHERE flush_lsn IS NOT NULL`).Scan(&lag)
	if err != nil {
		return -1
	}
	return lag
}

// computeRepoSideWALLag asks the agent's `wal list -o json`
// what segments the repo has committed, finds the highest
// end_lsn, and returns (pg_current_wal_lsn() - that, count).
// (-1, 0) on any error reaching the repo or PG; (0, n) when
// the repo is fully caught up.  Complements the live-side
// pg_stat_replication query — that one needs an active
// streamer connection, this one only needs the repo to be
// readable, so it stays meaningful between iterations.
func (d *DockerCellRuntime) computeRepoSideWALLag(ctx context.Context) (int64, int) {
	if d.conn == nil || d.AgentBinary == "" {
		return -1, 0
	}
	// `wal list` shells out via dockerExecCapture so we get
	// stdout cleanly (no stderr mixed in).
	stdout, _, err := d.dockerExecCapture(ctx,
		d.AgentBinary, "wal", "list", d.Deployment,
		"--repo", d.RepoURL,
		"-o", "json")
	if err != nil {
		return -1, 0
	}
	// CLI output shape: {"schema":..., "result": { "segments": [...] }}.
	// We tolerate missing fields (deployment with no segments yet
	// returns an empty list, which is the legitimate "wrote nothing"
	// case — surface as lag = pg_current_wal_lsn() relative to 0,
	// i.e. the full distance, with WALSegmentsCommitted = 0).
	var body struct {
		Result struct {
			Segments []struct {
				EndLSN string `json:"end_lsn"`
			} `json:"segments"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout, &body); err != nil {
		return -1, 0
	}

	var maxRepoLSN uint64
	for _, s := range body.Result.Segments {
		l, ok := parseLSN(s.EndLSN)
		if !ok {
			continue
		}
		if l > maxRepoLSN {
			maxRepoLSN = l
		}
	}

	var curStr string
	if err := d.conn.QueryRow(ctx, "select pg_current_wal_lsn()::text").Scan(&curStr); err != nil {
		return -1, 0
	}
	cur, ok := parseLSN(curStr)
	if !ok {
		return -1, 0
	}
	if cur < maxRepoLSN {
		// Implausible in practice (would mean the repo has
		// future LSNs the primary doesn't know about) but
		// guard against unsigned underflow.
		return 0, len(body.Result.Segments)
	}
	return int64(cur - maxRepoLSN), len(body.Result.Segments)
}

// parseLSN turns a PostgreSQL LSN string of the form "X/Y"
// (hex pair separated by "/") into a uint64 byte position.
// Returns (0, false) on any parse error so callers can
// distinguish "successfully parsed zero" from "unparseable".
func parseLSN(s string) (uint64, bool) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, false
	}
	hi, err := strconv.ParseUint(parts[0], 16, 32)
	if err != nil {
		return 0, false
	}
	lo, err := strconv.ParseUint(parts[1], 16, 32)
	if err != nil {
		return 0, false
	}
	return (hi << 32) | lo, true
}

// parsePgbenchSummary extracts (TPS, p95 latency in ms) from
// pgbench's final report block.  pgbench prints lines like:
//
//	tps = 1234.567 (without initial connection time)
//	latency stddev = 4.521 ms
//	latency average = 12.345 ms
//
// We pick the "tps =" line and convert latency_average as a
// stand-in for p95 (pgbench prints percentiles only with
// --latency-detail; we keep the report self-contained for
// now).  Future: switch to --latency-detail and parse the
// percentiles block when available.
func parsePgbenchSummary(s string) (tps, latencyMs float64) {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "tps = ") || strings.HasPrefix(line, "tps: "):
			parts := strings.Fields(strings.TrimPrefix(strings.TrimPrefix(line, "tps = "), "tps: "))
			if len(parts) > 0 {
				if v, err := strconv.ParseFloat(parts[0], 64); err == nil {
					tps = v
				}
			}
		case strings.HasPrefix(line, "latency average = "):
			parts := strings.Fields(strings.TrimPrefix(line, "latency average = "))
			if len(parts) > 0 {
				if v, err := strconv.ParseFloat(parts[0], 64); err == nil {
					latencyMs = v
				}
			}
		}
	}
	return tps, latencyMs
}

// --- helpers ----------------------------------------------------------

// dsn returns the host-perspective DSN for pgx.
func (d *DockerCellRuntime) dsn() string {
	user := d.PGUser
	if user == "" {
		user = "postgres"
	}
	db := d.PGDatabase
	if db == "" {
		db = "postgres"
	}
	return fmt.Sprintf("postgres://%s@127.0.0.1:%d/%s?sslmode=disable",
		user, d.HostPort, db)
}

// containerDSN returns the DSN the agent uses INSIDE the
// container — Unix-socket-style pointing at PG running on
// localhost:5432 from the agent's perspective.  Since the
// agent and PG share the container, this is straightforward.
func (d *DockerCellRuntime) containerDSN() string {
	user := d.PGUser
	if user == "" {
		user = "postgres"
	}
	db := d.PGDatabase
	if db == "" {
		db = "postgres"
	}
	return fmt.Sprintf("postgres://%s@127.0.0.1:5432/%s?sslmode=disable", user, db)
}

// exec returns an ExecFn bound to the runtime's pgx conn +
// the supplied context.  Wrapping pgx in this small layer lets
// schemas stay generic.
func (d *DockerCellRuntime) exec(ctx context.Context) ExecFn {
	return func(sql string, args ...any) error {
		if d.conn == nil {
			return errors.New("DockerCellRuntime: pgx conn not open")
		}
		_, err := d.conn.Exec(ctx, sql, args...)
		return err
	}
}

// dockerExec runs `docker exec <container> <argv...>` and
// returns COMBINED stdout+stderr.  Used by the error-diagnostic
// shell-outs (restore, pg_verifybackup, repo-init) where any
// chatter is what the operator wants to see when something fails.
//
// Do NOT use this when you need to PARSE stdout (e.g. JSON output
// from `pg_hardstorage backup -o json`) — combining stderr in
// breaks the parse the moment the agent or PG writes a single
// log line / NOTICE / progress event to stderr.  Use
// dockerExecCapture for those callsites and parse only stdout.
//
// (Regression history: when this used to be the only exec helper,
// TakeBackup's json.Unmarshal silently failed on every backup
// where any stderr noise was present, falling back to a
// synthesised backup ID and breaking the verify-restore step.)
func (d *DockerCellRuntime) dockerExec(ctx context.Context, argv ...string) ([]byte, error) {
	stdout, stderr, err := d.dockerExecCapture(ctx, argv...)
	combined := make([]byte, 0, len(stdout)+len(stderr))
	combined = append(combined, stdout...)
	combined = append(combined, stderr...)
	return combined, err
}

// isPGUnreachable returns true when stdout/stderr from a
// `pg_hardstorage backup` invocation indicates the connect-to-PG
// step failed with a transient network-level error.  Used by
// TakeBackup's retry loop to distinguish "PG isn't ready right
// now" (worth retrying) from "real bug" (propagate).  The
// matched substrings are the structured error code the agent
// emits + the libpq message body it wraps, both of which are
// stable across PG / pgx versions.
func isPGUnreachable(stdout, stderr []byte) bool {
	combined := append(append([]byte{}, stdout...), stderr...)
	if !bytesContains(combined, `"code": "storage.unreachable"`) &&
		!bytesContains(combined, `"code":"storage.unreachable"`) {
		return false
	}
	return bytesContains(combined, "cannot connect to PostgreSQL")
}

// bytesContains is a small allocation-free helper that avoids
// pulling in strings.Contains on byte slices (a strings.Contains
// would force a string conversion + copy).  Kept inline because
// this is the only caller.
func bytesContains(haystack []byte, needle string) bool {
	n := []byte(needle)
	for i := 0; i+len(n) <= len(haystack); i++ {
		if string(haystack[i:i+len(n)]) == needle {
			return true
		}
	}
	return false
}

// dockerExecCapture runs `docker exec -u pgbackup <container>
// <argv...>` with stdout and stderr captured into SEPARATE
// buffers.  Required for any callsite that parses stdout — JSON,
// structured output, etc.
//
// `-u pgbackup` matches production posture: the agent runs as a
// dedicated non-root system user, and the CLI gate at
// internal/cli/refuse_root.go now refuses euid 0 outright.  The
// testbed Dockerfiles create the pgbackup user and chown the
// agent dirs at build time; without `-u pgbackup` every agent
// invocation here would hit the refuse_root gate and exit 2.
//
// When the cell has a per-cell sink configured, the sink's
// EnvForAgent() is injected as `-e KEY=VAL` flags before the
// container name so the agent's storage layer (S3 SDK / Azure
// SDK / GCS SDK) sees the credentials.  Empty sinkAgentEnv
// (the no-sink case) leaves docker exec invocation unchanged
// and avoids any allocations beyond the existing append.
func (d *DockerCellRuntime) dockerExecCapture(ctx context.Context, argv ...string) ([]byte, []byte, error) {
	full := []string{"exec", "-u", "pgbackup"}
	for k, v := range d.sinkAgentEnv {
		full = append(full, "-e", k+"="+v)
	}
	full = append(full, d.Container)
	full = append(full, argv...)
	cmd := exec.CommandContext(ctx, d.dockerBin(), full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func (d *DockerCellRuntime) dockerBin() string {
	if d.DockerBin != "" {
		return d.DockerBin
	}
	return "docker"
}

// ensureRepoInit invokes `pg_hardstorage repo init` inside the
// container against d.RepoURL.  Idempotent: a previous run
// (or a sibling cell that won the race a few hundred ms ago)
// returns the structured `conflict.repo_exists` error which
// we treat as a successful no-op.
//
// We detect the conflict three ways, in priority order:
//  1. exit code 7 — the documented stable code for
//     "conflict (lease held, in-progress operation)".
//  2. substring match on `conflict.repo_exists` — robust
//     to JSON whitespace ("`code`": "..." with or without
//     space after the colon).
//  3. substring match on the human "already exists" text —
//     future-proofs against a renamed code.
//
// All three are belt-and-suspenders; any one is sufficient.
func (d *DockerCellRuntime) ensureRepoInit(ctx context.Context) error {
	out, err := d.dockerExec(ctx,
		d.AgentBinary, "repo", "init", d.RepoURL, "-o", "json")
	if err == nil {
		return nil
	}
	if isRepoAlreadyExists(err, out) {
		return nil
	}
	return fmt.Errorf("repo init at %s: %w (output: %s)",
		d.RepoURL, err, truncate(out, 512))
}

// isRepoAlreadyExists detects the repo-already-exists race on
// any of three signals: exit code 7, the structured error
// code, or the human message.  Whitespace-tolerant for the
// code match — JSON encoders disagree about " : " vs ":"
// across versions and we don't want to be sensitive to that.
func isRepoAlreadyExists(err error, output []byte) bool {
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 7 {
		return true
	}
	if bytes.Contains(output, []byte("conflict.repo_exists")) {
		return true
	}
	if bytes.Contains(output, []byte("a repository already exists at")) {
		return true
	}
	return false
}

// diagnoseContainer captures container state + recent logs
// for the runtime's lead container.  The result is a
// "\n--- diagnostics ... ---" block that the caller folds
// into the surrounding error.  Any underlying docker error
// becomes part of that block (we never want diagnostics to
// itself fail the call).
func (d *DockerCellRuntime) diagnoseContainer(ctx context.Context) string {
	if d.Container == "" {
		return ""
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "\n--- diagnostics for container %s ---\n", d.Container)
	// Container state — running? exited? exit code?
	state, _ := exec.CommandContext(ctx, d.dockerBin(),
		"inspect", "--format",
		"State.Status={{.State.Status}} ExitCode={{.State.ExitCode}} StartedAt={{.State.StartedAt}} OOMKilled={{.State.OOMKilled}}",
		d.Container).CombinedOutput()
	if len(state) > 0 {
		fmt.Fprintf(&b, "  inspect: %s", state)
	} else {
		fmt.Fprintln(&b, "  inspect: (no output — container missing?)")
	}
	// Last 200 log lines (stdout+stderr).  --tail caps the
	// volume, --timestamps gives us a timeline so the
	// operator can correlate with the soak driver's ND-JSON.
	logs, _ := exec.CommandContext(ctx, d.dockerBin(),
		"logs", "--tail", "200", "--timestamps",
		d.Container).CombinedOutput()
	if len(logs) > 0 {
		fmt.Fprintln(&b, "  --- last 200 log lines ---")
		// Indent each line so it's clearly part of the
		// diagnostic block in the eventual report.md.
		for _, line := range strings.Split(strings.TrimRight(string(logs), "\n"), "\n") {
			fmt.Fprintf(&b, "  | %s\n", line)
		}
	} else {
		fmt.Fprintln(&b, "  (no logs captured)")
	}
	fmt.Fprintln(&b, "--- end diagnostics ---")
	return b.String()
}

// waitForPG polls the supplied DSN until it accepts a
// connection or the timeout expires.  Returns the open
// connection on success.
func waitForPG(ctx context.Context, dsn string, timeout time.Duration) (*pgx.Conn, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, err := pgx.Connect(dialCtx, dsn)
		cancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("waitForPG: %w", lastErr)
}

// extractField finds the first occurrence of prefix and reads
// up to the first occurrence of suffix.  Used to extract a
// JSON field value from the agent's output without pulling in
// a full JSON parser for one match.
func extractField(s, prefix, suffix string) string {
	i := strings.Index(s, prefix)
	if i < 0 {
		return ""
	}
	rest := s[i+len(prefix):]
	j := strings.Index(rest, suffix)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// truncate caps a byte slice to n bytes for log lines.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
