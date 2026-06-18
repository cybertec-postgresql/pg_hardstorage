// Package runner executes a parsed scenario end-to-end: bring up the
// topology, run the load, take steps (backup, restore, inject faults),
// evaluate assertions, tear down.
//
// v0.1 implements the linear path. Steps run sequentially; assertions
// run in declaration order; failure of any step or assertion fails
// the whole run (exit code reflects which kind failed). The runner is
// deliberately small and inspectable — we expect operators to read it
// while debugging a flaky scenario.
package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers `pgx` driver

	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/assert"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/load"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/scenario"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/topology"
)

// RunOptions tunes a single scenario run.
type RunOptions struct {
	// Out is where progress NDJSON events go. nil = os.Stdout.
	Out io.Writer

	// ArtefactDir is where checkpoint files and result bundles land.
	// Empty = a temp dir under os.TempDir().
	ArtefactDir string

	// KeepOnFailure overrides scenario.Cleanup.OnFailure.
	KeepOnFailure bool

	// SkipTopology skips Up/Down — the runner uses the connection
	// string in the env var PG_HARDSTORAGE_TESTKIT_DSN. Useful when
	// debugging a scenario against an already-running PG.
	SkipTopology bool

	// Airgap refuses to fetch missing sink images at runtime.
	// When the scenario specifies a sink (Sink.Kind != ""), the
	// runner checks `docker image inspect` for the matching tag
	// BEFORE running anything; absent images become a pre-flight
	// error pointing at `image pull-sinks`.  Without --airgap a
	// missing image silently becomes a `docker pull` during sink
	// Up, which is the exact escape hatch air-gap mode prevents.
	Airgap bool
}

// Result is the structured outcome of one scenario run.
type Result struct {
	Schema        string          `json:"schema"`
	Scenario      string          `json:"scenario"`
	StartedAt     time.Time       `json:"started_at"`
	StoppedAt     time.Time       `json:"stopped_at"`
	DurationMS    int64           `json:"duration_ms"`
	StepResults   []StepResult    `json:"steps"`
	AssertResults []assert.Result `json:"asserts"`
	Pass          bool            `json:"pass"`
	Failure       string          `json:"failure,omitempty"`
}

// StepResult records the outcome of one step.
type StepResult struct {
	Index   int    `json:"index"`
	Kind    string `json:"kind"`
	Pass    bool   `json:"pass"`
	Message string `json:"message,omitempty"`
}

// ResultSchema is the schema identifier embedded in every
// Result NDJSON line.  24-month back-compat commitment via the
// same major-version contract as the other testkit schemas.
const ResultSchema = "pg_hardstorage.testkit.run.v1"

// Run executes a single scenario.
//
// Lifecycle:
//
//  1. Validate scenario already passed schema check via the parser.
//  2. Bring up topology (unless SkipTopology).
//  3. Open a *sql.DB to the primary.
//  4. Optionally apply the load file's ops once (the simple "drive PG
//     into a known state" mode; richer phase + duration semantics
//     land alongside the verifier subsystem).
//  5. Run each step in order.
//  6. Run scenario-level asserts.
//  7. Emit a Result NDJSON line, save it to ArtefactDir/result.ndjson,
//     return the typed Result.
func Run(ctx context.Context, sc *scenario.Scenario, opts RunOptions) (*Result, error) {
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	artefactDir := opts.ArtefactDir
	if artefactDir == "" {
		d, err := os.MkdirTemp("", "pg_hardstorage-testkit-")
		if err != nil {
			return nil, fmt.Errorf("runner: mkdir tempdir: %w", err)
		}
		artefactDir = d
	}
	if err := os.MkdirAll(artefactDir, 0o755); err != nil {
		return nil, fmt.Errorf("runner: mkdir %s: %w", artefactDir, err)
	}
	// Absolutise the artefact dir.  Steps derive on-disk paths and
	// file:// repo URLs from it (take_backup builds
	// file://<artefactDir>/repo); a relative --artefact-dir would
	// turn into a malformed file:// URL whose first path segment is
	// parsed as the URL host.  The MkdirTemp branch is already
	// absolute, but an explicit --artefact-dir may be relative.
	if abs, err := filepath.Abs(artefactDir); err == nil {
		artefactDir = abs
	}

	res := &Result{
		Schema:    ResultSchema,
		Scenario:  sc.Name,
		StartedAt: time.Now().UTC(),
	}

	emit(opts.Out, "scenario.started", map[string]any{
		"name":     sc.Name,
		"tier":     sc.Tier,
		"provider": sc.Topology.Provider,
	})

	// Sink: bring up the storage-backend container BEFORE
	// the topology so the agent's first invocation has a
	// repo URL to dial.  Empty SinkSpec keeps file:// under
	// the artefact dir (legacy, no container needed).
	//
	// The defer pairs the Down with a fresh context — the
	// soak-side ctx may already be cancelled by the time we
	// tear down, and we'd rather over-clean (rm a container
	// that's already gone) than leak.
	var sinkRT sink.Runtime
	if sc.Sink.Kind != "" {
		// Air-gap pre-flight: refuse to even try Up if the
		// sink image isn't already local.  Translates the
		// network requirement into a fail-fast error that
		// names the exact pull-sinks command to fix it.
		if opts.Airgap {
			if err := sink.PreflightAirgap(ctx, sc.Sink.Kind); err != nil {
				res.Failure = err.Error()
				res.finish(opts.Out, artefactDir)
				return res, err
			}
		}
		s, err := sink.New(sc.Sink.Kind)
		if err != nil {
			res.Failure = err.Error()
			res.finish(opts.Out, artefactDir)
			return res, err
		}
		sinkRT = s
		emit(opts.Out, "sink.up.starting", map[string]any{"kind": s.Name()})
		if err := s.Up(ctx); err != nil {
			res.Failure = "sink: " + err.Error()
			res.finish(opts.Out, artefactDir)
			return res, err
		}
		emit(opts.Out, "sink.up.completed", map[string]any{
			"kind": s.Name(),
			"url":  s.URL(),
		})
		defer func() {
			downCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := sinkRT.Down(downCtx); err != nil {
				emit(opts.Out, "sink.down.error", map[string]any{"error": err.Error()})
			}
		}()
	}

	dsn := os.Getenv("PG_HARDSTORAGE_TESTKIT_DSN")
	var topo topology.Topology
	if !opts.SkipTopology {
		t, err := topology.Build(sc.Topology.Provider)
		if err != nil {
			res.Failure = err.Error()
			res.finish(opts.Out, artefactDir)
			return res, err
		}
		topo = t
		emit(opts.Out, "topology.up.starting", map[string]any{"provider": topo.Name()})
		// Pre-flight: when the scenario pins a locally-built image
		// (no registry prefix like ghcr.io/ or docker.io/),
		// confirm it exists locally before letting topology.Up
		// shell to docker and get the generic "pull access denied"
		// error.  Surfaces L4_pg_upgrade_cross_major's missing-image
		// case as a fast actionable error instead of a confusing
		// 500ms scenario fail.
		if img := sc.Topology.Image; img != "" && !strings.ContainsAny(img, "/") {
			if !dockerImageExistsLocally(ctx, img) {
				msg := fmt.Sprintf("topology image %q not found locally; this scenario uses a single-purpose testbed image that is not in the published registry. "+
					"Build it first: `make build-multipg-image` (or use the docker build invocation in the scenario YAML's comment)", img)
				res.Failure = msg
				res.finish(opts.Out, artefactDir)
				return res, fmt.Errorf("%s", msg)
			}
		}
		if err := topo.Up(ctx, topology.UpOptions{
			PGVersion:     sc.Topology.PGVersion,
			Image:         sc.Topology.Image,
			Filesystem:    sc.Topology.Filesystem,
			Operator:      sc.Topology.Operator,
			Replicas:      sc.Topology.Replicas,
			InventoryFile: sc.Topology.InventoryRef,
			ExtraGUCs:     sc.Topology.ExtraGUCs,
		}); err != nil {
			res.Failure = err.Error()
			res.finish(opts.Out, artefactDir)
			return res, err
		}
		dsn = topo.ConnString()
		defer func() {
			tearCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := topo.Down(tearCtx); err != nil {
				emit(opts.Out, "topology.down.error", map[string]any{"error": err.Error()})
			}
		}()
	}

	if dsn == "" {
		res.Failure = "no DSN — topology produced none and PG_HARDSTORAGE_TESTKIT_DSN unset"
		res.finish(opts.Out, artefactDir)
		return res, errors.New(res.Failure)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		res.Failure = err.Error()
		res.finish(opts.Out, artefactDir)
		return res, err
	}
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()

	// Verify the connection actually works before running anything.
	pingCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		res.Failure = "PG ping failed: " + err.Error()
		res.finish(opts.Out, artefactDir)
		return res, err
	}

	// 4. Load. v0.1 supports a flat "apply every operation in every
	// phase, in order" pass. Mix / target_qps / duration honoured by
	// once the workload-driver lands.
	if sc.Load.File != "" {
		if err := applyLoad(ctx, db, sc.Load.File, opts.Out); err != nil {
			res.Failure = "load: " + err.Error()
			res.finish(opts.Out, artefactDir)
			return res, err
		}
	}

	// Build the per-run state that step handlers thread through.
	// agentBin / repo URL are resolved here once; lastBackup
	// updates as take_backup runs; targets come from the
	// topology so an `inject` step can dispatch into the inject
	// fault registry.
	// connString returns the freshest DSN — for topologies with
	// dynamic leaders (e.g. patroni-local-docker) it polls
	// /leader on every call, so a step that runs after an
	// inject patroni_failover automatically reaches the new
	// primary instead of the previous-leader-now-replica
	// (which would reject DDL with "cannot execute ... in a
	// read-only transaction").  When SkipTopology is set, the
	// closure just returns the env-var DSN unchanged.
	connStringFn := func() string { return dsn }
	if topo != nil {
		connStringFn = topo.ConnString
	}
	repoURL := "file://" + filepath.Join(artefactDir, "repo")
	var agentEnv map[string]string
	if sinkRT != nil {
		// Sink-driven repo URL replaces the file:// default.
		// agentEnv is merged into every shell-out's env so the
		// agent's storage layer (S3 SDK / Azure SDK / ...)
		// finds its credentials.
		repoURL = sinkRT.URL()
		agentEnv = sinkRT.EnvForAgent()
	}
	// Per-scenario HOME so persistent agent state (deployment
	// registry, logical_streams.json, timetravel.json, keyring)
	// is fresh on every run.  Without this, a re-run of the same
	// scenario hits conflict.deployment_exists / conflict.logical_exists
	// against state left behind by a prior run.  XDG_*_HOME + HOME
	// pinned to artefactDir/home keep the agent's path resolver
	// inside the scenario's sandbox.
	if agentEnv == nil {
		agentEnv = map[string]string{}
	}
	scenarioHome := filepath.Join(artefactDir, "home")
	_ = os.MkdirAll(scenarioHome, 0o700)
	agentEnv["HOME"] = scenarioHome
	agentEnv["XDG_CONFIG_HOME"] = filepath.Join(scenarioHome, ".config")
	agentEnv["XDG_DATA_HOME"] = filepath.Join(scenarioHome, ".local", "share")
	agentEnv["XDG_STATE_HOME"] = filepath.Join(scenarioHome, ".local", "state")

	// Pre-scan steps for the scenario's primary deployment
	// name.  wal_stream and take_backup both archive into
	// repo paths keyed on this name; if they disagree (e.g.
	// wal_stream uses the default "scenario" but take_backup
	// uses "l3-pitr-forward"), restore_command's wal fetch
	// looks under the take_backup's name and fails to find
	// streamed segments.  Picking the first explicit
	// Deployment we see — typically take_backup's — keeps
	// the two in sync without requiring the operator to
	// repeat the name on every step.
	scenarioDeployment := "scenario"
	for _, st := range sc.Steps {
		if st.Deployment != "" {
			scenarioDeployment = st.Deployment
			break
		}
	}
	state := &runState{
		artefactDir: artefactDir,
		pgConn:      dsn,
		connString:  connStringFn,
		deployment:  scenarioDeployment,
		repoURL:     repoURL,
		agentEnv:    agentEnv,
		loadFile:    sc.Load.File,
		pgVersion:   sc.Topology.PGVersion,
	}
	// Defensive cleanup for the wal_stream sidecar: a scenario
	// that forgets a `wal_stream: { action: stop }` after a
	// failed step would leak the streamer process.  We stop it
	// here on the way out regardless of how Run exits.
	defer func() {
		if state.walStreamCmd == nil {
			return
		}
		state.walStreamCancel()
		select {
		case <-state.walStreamDone:
		case <-time.After(5 * time.Second):
		}
	}()
	// agentBin lookup is deferred to the first step that needs
	// it: scenarios that exercise only load + assert (the v0.1
	// L1 smoke shape) shouldn't need the agent binary at all,
	// and we'd rather not block them on a missing PG_HARDSTORAGE_BIN.
	// Resolution happens lazily via ensureAgentBin.
	if topo != nil {
		ts := topo.Targets()
		// Register the sink container as a `sink`-role inject
		// target when the scenario brings one up.  The L3-storage-
		// outage scenario relies on docker_pause(target=sink) to
		// freeze the storage emulator mid-flight; without this
		// registration the inject step can't find the sink in
		// the TargetSet and aborts with "no targets with role
		// 'sink'".  Mirrors the soak driver's DockerCellRuntime
		// which registers the same role.
		if sinkRT != nil && sinkRT.ContainerName() != "" {
			ts = append(ts, &inject.DockerTarget{
				Container: sinkRT.ContainerName(),
				RoleStr:   "sink",
			})
		}
		if len(ts) > 0 {
			// The seed for *_random target picks is derived
			// from the scenario start time — different runs
			// explore different fault → target combinations.
			// Tests that need determinism set the seed via
			// PG_HARDSTORAGE_TESTKIT_SEED.
			seed := res.StartedAt.UnixNano()
			if v := os.Getenv("PG_HARDSTORAGE_TESTKIT_SEED"); v != "" {
				if n, perr := strconvParseInt64(v); perr == nil {
					seed = n
				}
			}
			state.targets = inject.NewStaticTargetSet(ts, seed)
		}
		emit(opts.Out, "topology.targets", map[string]any{
			"summary": formatTargetSummary(ts),
		})
	}

	// 5. Steps.
	for i, st := range sc.Steps {
		sr := runStep(ctx, db, st, i, state, opts.Out)
		res.StepResults = append(res.StepResults, sr)
		if !sr.Pass {
			res.Failure = fmt.Sprintf("step %d (%s): %s", i, st.Kind, sr.Message)
			res.finish(opts.Out, artefactDir)
			return res, errors.New(res.Failure)
		}
	}

	// 6. Scenario-level asserts.
	if len(sc.Asserts) > 0 {
		// Refresh `db` against the topology's current DSN.  An
		// inject step earlier in the scenario may have bounced PG
		// — testcontainers re-randomises the `0:5432` host port
		// across docker stop+start, so the cached pool's conns
		// all point at the pre-bounce port.  See state.currentDSN
		// commentary for the same dance in switch_wal / run_load.
		if fresh, oerr := sql.Open("pgx", connStringFn()); oerr == nil {
			_ = db.Close()
			db = fresh
		}
		ar, _ := assert.RunAll(ctx, assert.Context{DB: db}, sc.Asserts)
		res.AssertResults = ar
		for _, r := range ar {
			if !r.Passed {
				res.Failure = fmt.Sprintf("assert %s: %s", r.Kind, r.Message)
				res.finish(opts.Out, artefactDir)
				return res, errors.New(res.Failure)
			}
		}
	}

	res.Pass = true
	res.finish(opts.Out, artefactDir)
	return res, nil
}

func (r *Result) finish(out io.Writer, dir string) {
	r.StoppedAt = time.Now().UTC()
	r.DurationMS = r.StoppedAt.Sub(r.StartedAt).Milliseconds()

	// Emit + save the result file.
	emit(out, "scenario.completed", map[string]any{
		"pass":         r.Pass,
		"duration_ms":  r.DurationMS,
		"step_count":   len(r.StepResults),
		"assert_count": len(r.AssertResults),
		"failure":      r.Failure,
	})

	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		emit(out, "result.marshal_failed", map[string]any{"error": err.Error()})
		return
	}
	resultPath := filepath.Join(dir, "result.json")
	// fsutil.WriteFileSync: testkit result.json is the canonical
	// success/failure record CI scripts read.  Worth fsync-ing so
	// a crash between "test passed" and the runner's exit doesn't
	// strand a half-written or invisible result file.
	if werr := fsutil.WriteFileSync(resultPath, body, 0o644); werr != nil {
		// Disk-full / read-only fs / permission errors during a
		// testkit run would otherwise lose result.json silently.
		// Surface via the NDJSON event stream so CI logs capture it.
		emit(out, "result.write_failed", map[string]any{
			"path":  resultPath,
			"error": werr.Error(),
		})
	}
}

func runStep(ctx context.Context, db *sql.DB, st scenario.Step, i int, state *runState, out io.Writer) StepResult {
	emit(out, "step.starting", map[string]any{"index": i, "kind": st.Kind})
	switch st.Kind {
	case "switch_wal":
		// Force a WAL rotation so any in-flight segment is
		// sealed and visible to a streaming wal-archiver.
		// Used by PITR scenarios that capture an LSN inside
		// an in-flight segment: without this rotation,
		// `wal_stream stop` cannot commit the segment to the
		// repo (the streamer only finalises a segment when
		// PG writes a record into the successor segment),
		// and the subsequent restore --to-lsn aborts with
		// "recovery ended before configured recovery target
		// was reached".
		//
		// Re-resolve the DSN per call (don't reuse the
		// scenario-start *sql.DB).  After a patroni_failover
		// the scenario-start leader is now a replica, and
		// pg_switch_wal() on a replica errors with `recovery
		// is in progress (SQLSTATE 55000)`.  state.currentDSN()
		// polls /leader so we always reach the writable
		// primary.  Same shape run_load already uses (see
		// steps.go:runLoad — re-resolve DSN every pass).
		swDSN := state.currentDSN()
		if swDSN == "" {
			return StepResult{Index: i, Kind: st.Kind, Pass: false,
				Message: "switch_wal: topology returned empty DSN (no leader reachable)"}
		}
		swDB, openErr := sql.Open("pgx", swDSN)
		if openErr != nil {
			return StepResult{Index: i, Kind: st.Kind, Pass: false,
				Message: "switch_wal: open: " + openErr.Error()}
		}
		defer swDB.Close()
		var switchLSN string
		if err := swDB.QueryRowContext(ctx, "SELECT pg_switch_wal()::text").Scan(&switchLSN); err != nil {
			return StepResult{Index: i, Kind: st.Kind, Pass: false,
				Message: "switch_wal: " + err.Error()}
		}
		// Two follow-ups, both required for `wal_stream stop` to
		// actually commit the just-closed segment to the repo:
		//
		// (1) CHECKPOINT — pg_switch_wal seals the OLD segment but
		//     the streamer's walsink only finalises a segment when a
		//     record lands in the NEXT segment (the gap-detection
		//     invariant — see internal/pg/walsink/walsink.go OnRecord).
		//     An idle PG between the switch and `wal_stream stop`
		//     leaves the new segment empty, walsink keeps the just-
		//     closed segment buffered in memory, the SIGTERM-graceful
		//     stop times out at its 5 s SyncedLSN-poll deadline, and
		//     the segment never hits the repo.  CHECKPOINT writes a
		//     checkpoint record into the new segment, satisfying the
		//     successor-record requirement.  Reproduced 2026-05-12 in
		//     L3_wal_stream_ddl_storm: PITR target inside the just-
		//     closed segment failed with "recovery ended before
		//     configured recovery target was reached".
		//
		// (2) Wait for the streamer's slot's confirmed_flush_lsn to
		//     advance past the switch LSN.  PG records the
		//     replication slot's view of how far the streamer has
		//     acked, so when this advances we know walsink has
		//     finalised the segment (its final SyncedLSN ack flowed
		//     back to PG).  Without this wait, a fast `wal_stream
		//     stop` immediately after `switch_wal` races the in-flight
		//     commit.  60 s deadline is comfortable for a 16 MiB
		//     segment under typical scenario load; soft-fail past it
		//     so a misconfigured slot doesn't deadlock the scenario.
		if _, err := swDB.ExecContext(ctx, "CHECKPOINT"); err != nil {
			return StepResult{Index: i, Kind: st.Kind, Pass: false,
				Message: "switch_wal: checkpoint after switch: " + err.Error()}
		}
		waitDeadline := time.Now().Add(60 * time.Second)
		waitedSlot := ""
		waitedFlush := ""
		for time.Now().Before(waitDeadline) {
			var slotName, flushLSN sql.NullString
			err := swDB.QueryRowContext(ctx,
				"SELECT slot_name, confirmed_flush_lsn::text FROM pg_replication_slots "+
					"WHERE slot_type = 'physical' AND active = true "+
					"ORDER BY slot_name LIMIT 1").Scan(&slotName, &flushLSN)
			if err == nil && slotName.Valid && flushLSN.Valid {
				waitedSlot = slotName.String
				waitedFlush = flushLSN.String
				if lsnGTE(flushLSN.String, switchLSN) {
					break
				}
			}
			select {
			case <-ctx.Done():
				return StepResult{Index: i, Kind: st.Kind, Pass: false,
					Message: "switch_wal: ctx cancelled while waiting for slot to catch up"}
			case <-time.After(200 * time.Millisecond):
			}
		}
		emit(out, "step.switch_wal.completed", map[string]any{
			"index":        i,
			"switch_lsn":   switchLSN,
			"waited_slot":  waitedSlot,
			"waited_flush": waitedFlush,
		})
		return StepResult{Index: i, Kind: st.Kind, Pass: true,
			Message: "WAL rotated"}
	case "assert":
		// Refresh `db` against state.currentDSN() — an earlier
		// inject step may have bounced PG to a new host port
		// (testcontainers re-randomises `0:5432` on docker
		// stop+start).  See the scenario-asserts site for the
		// same pattern.
		if dsnNow := state.currentDSN(); dsnNow != "" {
			if fresh, oerr := sql.Open("pgx", dsnNow); oerr == nil {
				_ = db.Close()
				db = fresh
			}
		}
		ar, err := assert.RunAll(ctx, assert.Context{DB: db}, st.Asserts)
		if err != nil {
			return StepResult{Index: i, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("%d/%d asserts failed", failedCount(ar), len(ar))}
		}
		return StepResult{Index: i, Kind: st.Kind, Pass: true,
			Message: fmt.Sprintf("%d/%d asserts passed", len(ar), len(ar))}
	case "run_load":
		return runLoad(ctx, db, st, i, state, out)
	case "sql":
		return runSQLStep(ctx, st, i, state, out)
	case "seed":
		return runSeed(ctx, st, i, state, out)
	case "wal_stream":
		// wal_stream shells out to `pg_hardstorage wal stream`
		// AND triggers a lazy repo init on first use, both of
		// which need state.agentBin populated.  Without this
		// resolve the runner would fail with the cryptic
		// "exec: no command" — argv[0] is empty.
		if err := ensureAgentBin(state); err != nil {
			return StepResult{Index: i, Kind: st.Kind, Pass: false,
				Message: err.Error()}
		}
		return runWALStream(ctx, st, i, state, out)
	case "drop_slot":
		return runDropSlot(ctx, st, i, state, out)
	case "wait_wal_archived":
		return runWaitWALArchived(ctx, st, i, state, out)
	case "capture_lsn":
		return runCaptureLSN(ctx, st, i, state, out)
	case "assert_restored_match":
		// The sandbox bind-mounts state.agentBin into the
		// container so the basebackup's restore_command (which
		// the agent emits with its own absolute path) resolves
		// inside the sandbox.  Resolve lazily here — scenarios
		// that only do load + assert don't need the binary, but
		// any scenario reaching assert_restored_match has
		// already produced a backup that names the agent path.
		if err := ensureAgentBin(state); err != nil {
			return StepResult{Index: i, Kind: st.Kind, Pass: false,
				Message: err.Error()}
		}
		return runAssertRestoredMatch(ctx, st, i, state, out)
	case "take_backup":
		if err := ensureAgentBin(state); err != nil {
			return StepResult{Index: i, Kind: st.Kind, Pass: false,
				Message: err.Error()}
		}
		return runTakeBackup(ctx, st, i, state, out)
	case "restore":
		if err := ensureAgentBin(state); err != nil {
			return StepResult{Index: i, Kind: st.Kind, Pass: false,
				Message: err.Error()}
		}
		return runRestore(ctx, st, i, state, out)
	case "inject":
		return runInject(ctx, st, i, state, out)
	case "compat_archive":
		// Compat-shim end-to-end driver — runs
		// pg-hardstorage-pgbackrest /
		// pg-hardstorage-barman-wal-archive inside the
		// target OS container against a synthetic
		// segment + .backup companion, asserts the repo
		// manifests landed.  Drives the shim through its
		// REAL binary (not a mock dispatcher), so this is
		// what catches dispatch bugs of the kind the
		// barman shim shipped before this commit.
		return runCompatArchive(ctx, st, i, state, out)
	case "compat_doppelganger":
		// Split-brain archive collision: push a segment,
		// then push another segment with the SAME name +
		// SAME system_identifier but DIFFERENT content.
		// Two clusters with a cloned datadir + shared
		// archive_command produce exactly this race.  We
		// require the second push to surface a structured
		// error (splitbrain.content_mismatch); without
		// detection, archive_command's idempotent-rename
		// path silently treats the loser as success.
		return runCompatDoppelganger(ctx, st, i, state, out)
	case "restored_load":
		// Boot the most-recently-restored datadir in a
		// postgres:<version> sandbox and run pgbench
		// against it for the configured duration.  The
		// "does PG actually start and serve traffic" gate —
		// catches restore-correctness bugs that
		// pg_verifybackup misses (catalog corruption,
		// missing FSM/VM, broken statistics).
		if err := ensureAgentBin(state); err != nil {
			return StepResult{Index: i, Kind: st.Kind, Pass: false,
				Message: err.Error()}
		}
		return runRestoredLoad(ctx, st, i, state, out)
	case "corrupt_repo_object":
		// Surgical mutation of stored repo state, paired
		// with an assertion that the next op detects the
		// corruption.  Adversarial test for the integrity
		// invariants — chunk content drift, manifest
		// tampering, HSREPO mode flip — each must produce
		// a structured verify.* error, never silent
		// garbage.
		return runCorruptRepoObject(ctx, st, i, state, out)
	case "cli_run":
		// Generic shell-out to the pg_hardstorage binary —
		// the escape hatch for E2E-testing CLI surfaces
		// the runner doesn't have a dedicated step for
		// (hold, kms, audit, recovery, verify, classify,
		// anomaly, ...).  See runCLIRun for placeholder
		// substitution + assertion semantics.
		if err := ensureAgentBin(state); err != nil {
			return StepResult{Index: i, Kind: st.Kind, Pass: false,
				Message: err.Error()}
		}
		return runCLIRun(ctx, st, i, state, out)

	// ---- L4 upgrade/compat steps ----
	//
	// swap_binary, synthesize_manifest, write_repo_marker,
	// os_pkg_upgrade, pg_upgrade, swap_pg_minor.  These are
	// all small wrappers around `docker cp` + `docker exec` +
	// file writes; they live in steps_l4.go to keep the
	// runner.go dispatch readable.  See steps_l4.go for the
	// per-step rationale + idempotency contract.
	case "swap_binary":
		return runSwapBinary(ctx, st, i, state, out)
	case "synthesize_manifest":
		return runSynthesizeManifest(ctx, st, i, state, out)
	case "write_repo_marker":
		return runWriteRepoMarker(ctx, st, i, state, out)
	case "os_pkg_upgrade":
		return runOSPkgUpgrade(ctx, st, i, state, out)
	case "pg_upgrade":
		return runPgUpgrade(ctx, st, i, state, out)
	case "swap_pg_minor":
		return runSwapPgMinor(ctx, st, i, state, out)
	}
	return StepResult{Index: i, Kind: st.Kind, Pass: false,
		Message: "unknown step kind"}
}

// ensureAgentBin lazily resolves the pg_hardstorage binary for the
// step handlers that shell out to it.  Lookup happens at most
// once per scenario; a missing binary fails the step with a clear
// error pointing at the env var override.
func ensureAgentBin(state *runState) error {
	if state.agentBin != "" {
		return nil
	}
	bin, err := resolveAgentBinary()
	if err != nil {
		return err
	}
	state.agentBin = bin
	return nil
}

// strconvParseInt64 is a thin wrapper kept private so the runner
// doesn't pull strconv into runner.go for one call site.
func strconvParseInt64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

func failedCount(rs []assert.Result) int {
	n := 0
	for _, r := range rs {
		if !r.Passed {
			n++
		}
	}
	return n
}

// applyLoad walks every phase's Operations once. Mix / target_qps are
// not honoured in v0.1 (the runner doesn't yet do concurrent driving);
// scenarios that need them mark themselves tier: L3+ which doesn't
// run in v0.1's L1/L2 CI.
func applyLoad(ctx context.Context, db *sql.DB, path string, out io.Writer) error {
	l, err := load.LoadFromFile(path)
	if err != nil {
		return err
	}
	emit(out, "load.starting", map[string]any{"file": path, "seed": l.Seed, "phases": len(l.Phases)})
	for _, ph := range l.Phases {
		emit(out, "load.phase.starting", map[string]any{"phase": ph.Name})
		for _, op := range ph.Operations {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := applyOp(ctx, db, op, out); err != nil {
				return fmt.Errorf("phase %q op %s: %w", ph.Name, op.Kind, err)
			}
		}
	}
	return nil
}

// applyOp runs one declarative load operation. v0.1 handles the basic
// schema/data-shape ops. We keep applyOp small and switch-based so a
// future contributor can add an op by adding one case.
func applyOp(ctx context.Context, db *sql.DB, op load.Operation, out io.Writer) error {
	switch op.Kind {
	case "create_table":
		return runCreateTable(ctx, db, op)
	case "insert_rows":
		return runInsertRows(ctx, db, op, out)
	case "create_index":
		return runCreateIndex(ctx, db, op)
	case "vacuum":
		return runVacuum(ctx, db, op)
	case "checkpoint":
		emit(out, "load.checkpoint", map[string]any{"label": op.Label})
		return nil
	}
	return fmt.Errorf("unsupported op kind %q (v0.1 supports create_table, insert_rows, create_index, vacuum, checkpoint)", op.Kind)
}

// emit writes a single NDJSON line to out. Errors are swallowed — the
// observability stream is best-effort by design (failing to log
// shouldn't fail a scenario).
func emit(out io.Writer, name string, body map[string]any) {
	body["event"] = name
	body["at"] = time.Now().UTC().Format(time.RFC3339Nano)
	enc, _ := json.Marshal(body)
	_, _ = out.Write(append(enc, '\n'))
}

// lsnGTE reports whether `a` (an LSN in PG `X/Y` text form) is >= `b`.
// Used by switch_wal's wait-for-slot loop to compare a slot's
// confirmed_flush_lsn against the just-completed switch LSN.
//
// Bypasses any new dependency just for one comparison: parses the
// `<hi>/<lo>` text into a pair of uint64s and compares
// lexicographically.  Both halves are hex.  An unparseable input
// returns false so the caller treats the slot as "not yet caught up"
// and keeps polling.
func lsnGTE(a, b string) bool {
	ah, al, aok := splitLSN(a)
	bh, bl, bok := splitLSN(b)
	if !aok || !bok {
		return false
	}
	if ah != bh {
		return ah > bh
	}
	return al >= bl
}

func splitLSN(s string) (hi, lo uint64, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			h, e1 := strconv.ParseUint(s[:i], 16, 64)
			l, e2 := strconv.ParseUint(s[i+1:], 16, 64)
			if e1 != nil || e2 != nil {
				return 0, 0, false
			}
			return h, l, true
		}
	}
	return 0, 0, false
}

// dockerImageExistsLocally reports whether `docker image inspect`
// finds the image.  Used as a pre-flight when the scenario's
// topology pins a locally-built image (no registry prefix) — gives
// the operator a fast, actionable error if the image is missing
// instead of letting topology.Up's docker call fail with the
// generic "pull access denied".
func dockerImageExistsLocally(ctx context.Context, image string) bool {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", image)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}
