// restored_load step — the "does PG actually start and serve
// traffic" gate.
//
// pg_verifybackup proves the manifest matches the bytes; assert_
// restored_match proves the byte-equality of the restored datadir
// against a captured live-cluster checksum.  Neither catches:
//
//   - The datadir is byte-perfect but PG won't start
//     (postgresql.conf references a missing user, recovery.signal
//     malformed, control file inconsistent post-promote, ...).
//   - PG starts but serves errors on every query (catalog
//     corruption that survives initdb, broken statistics,
//     missing FSM/VM files).
//   - PG serves correctly but with material performance
//     regression (restored bloat, empty hint-bit cache, ...).
//
// This step closes the gap by booting the restored datadir in a
// fresh postgres:<version> sandbox container, waiting until psql
// accepts connections, running pgbench against it for the
// configured duration, and asserting:
//
//   - pgbench exited cleanly,
//   - the failed-transaction count ≤ PgbenchErrorTolerance,
//   - the reported TPS ≥ PgbenchTPSFloor.
//
// Reuses startRestoredSandbox from steps.go (the same sandbox
// boot path assert_restored_match uses), so the agent binary +
// repo paths are already plumbed for restore_command's WAL
// fetch fallback during recovery.

package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/scenario"
)

// runRestoredLoad implements the restored_load step.  Resolves
// the target datadir (capture-state name → state.lastRestoreTarget
// → error), boots a sandbox, runs pgbench, asserts the
// post-restore cluster behaves as a real cluster.
func runRestoredLoad(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	target, err := resolveRestoredTarget(st, state)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false, Message: err.Error()}
	}
	if state.pgVersion == "" {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "restored_load: scenario.topology.pg_version is empty (needed to pick the postgres:<version> image)"}
	}

	clients := st.PgbenchClients
	if clients <= 0 {
		clients = 4
	}
	durStr := strings.TrimSpace(st.Duration)
	if durStr == "" {
		durStr = "30s"
	}
	dur, err := time.ParseDuration(durStr)
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("restored_load: duration %q: %v", durStr, err)}
	}
	if dur <= 0 {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "restored_load: duration must be > 0"}
	}

	// Sandbox shape mirrors assert_restored_match: a
	// postgres:<version> container running as the host's
	// uid:gid, with the datadir bind-mounted at
	// /var/lib/postgresql/data + the agent binary at the
	// embedded restore_command path.
	sandboxName := fmt.Sprintf("pg-hs-restored-load-%d-%d", idx,
		time.Now().UnixNano()%1_000_000)
	sandboxUser := pgUserFromDSN(state.pgConn)
	if sandboxUser == "" {
		sandboxUser = "postgres"
	}
	sandboxPass := pgPasswordFromDSN(state.pgConn)
	sandboxDB := pgDBFromDSN(state.pgConn)
	if sandboxDB == "" {
		sandboxDB = "postgres"
	}

	stop, err := startRestoredSandbox(ctx, sandboxName, target, state.pgVersion,
		state.agentBin, state.repoURL, state.agentEnv)
	if err != nil {
		// Same forensic dance as assert_restored_match: scrape
		// docker logs into the artefact dir before tearing down
		// so a CI run captures why the sandbox refused to come up.
		logsOut, _ := exec.Command("docker", "logs", "--tail", "200", sandboxName).CombinedOutput()
		_ = exec.Command("docker", "rm", "-f", sandboxName).Run()
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("restored_load: sandbox start: %v (logs: %s)", err, truncate(logsOut, 384))}
	}
	defer stop()

	// Poll for `select 1` ready.  The sandbox boots PG in
	// recovery mode (replay WAL up to consistency); the wait
	// budget covers replay of the basebackup's IncludeWAL
	// payload.  30 s matches assert_restored_match's window.
	ready := false
	for i := 0; i < 30; i++ {
		check := exec.CommandContext(ctx, "docker", "exec", sandboxName,
			"psql", "-U", sandboxUser, "-At", "-c", "select 1")
		if check.Run() == nil {
			ready = true
			break
		}
		select {
		case <-ctx.Done():
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: "restored_load: ctx cancelled while waiting for sandbox"}
		case <-time.After(time.Second):
		}
	}
	if !ready {
		logsOut, _ := exec.Command("docker", "logs", "--tail", "200", sandboxName).CombinedOutput()
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("restored_load: sandbox did not accept connections within 30s (logs: %s)", truncate(logsOut, 512))}
	}

	// `select 1` succeeds during recovery too — but pgbench's
	// setup uses VACUUM and TRUNCATE which fail with
	//   ERROR:  cannot execute VACUUM during recovery
	//   ERROR:  cannot execute TRUNCATE TABLE in a read-only
	//          transaction
	// Default recovery_target_action is `pause` (the restore
	// command's default), which leaves the sandbox in recovery
	// forever after WAL replay completes.  Trigger promote
	// explicitly and wait for `pg_is_in_recovery() = false`
	// before running pgbench.
	if err := promoteRestoredSandbox(ctx, sandboxName, sandboxUser); err != nil {
		logsOut, _ := exec.Command("docker", "logs", "--tail", "200", sandboxName).CombinedOutput()
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("restored_load: promote: %v (logs: %s)", err, truncate(logsOut, 384))}
	}

	pgBinDir, err := containerPGBin(ctx, sandboxName, "pgbench")
	if err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("restored_load: locate pgbench in sandbox: %v", err)}
	}

	// pgbench against pre-existing tables: -i would re-init
	// (drop + create), destroying the restored data; we
	// invoke without -i so it runs the standard TPC-B-like
	// mix against whatever schema the seeded scenario put in
	// place.  PGPASSWORD lands in the in-container shell's
	// env only — never in the docker argv (where ps would
	// expose it).
	durSec := int(dur.Seconds())
	if durSec < 1 {
		durSec = 1
	}
	// No `-p`: docker exec inherits the container's PGPORT env
	// (set when the sandbox was started via startRestoredSandbox's
	// dynamic free-port pick — d44842c).  pgbench reads PGPORT
	// automatically.  Hard-coding `-p 5432` here used to work
	// because the sandbox bound 5432, but after the port-collision
	// fix the sandbox now binds a dynamic port and the hard-coded
	// 5432 connect refused.
	shellCmd := fmt.Sprintf(
		"PGPASSWORD=%q %s/pgbench -c %d -j %d -T %d -h 127.0.0.1 -U %s -d %s",
		sandboxPass, pgBinDir, clients, clients, durSec,
		shellQuote(sandboxUser), shellQuote(sandboxDB),
	)
	emit(out, "step.restored_load.starting", map[string]any{
		"index":    idx,
		"target":   target,
		"clients":  clients,
		"duration": durStr,
	})
	cmd := exec.CommandContext(ctx, "docker", "exec", sandboxName, "sh", "-c", shellCmd)
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	if err := cmd.Run(); err != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("restored_load: pgbench: %v (output: %s)", err, truncate(combined.Bytes(), 512))}
	}

	tps, errCount, parseErr := parsePgbenchOutput(combined.String())
	if parseErr != nil {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("restored_load: parse pgbench output: %v (output: %s)", parseErr, truncate(combined.Bytes(), 512))}
	}
	if errCount > st.PgbenchErrorTolerance {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("restored_load: %d failed transactions exceeds tolerance %d", errCount, st.PgbenchErrorTolerance)}
	}
	if st.PgbenchTPSFloor > 0 && tps < st.PgbenchTPSFloor {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("restored_load: TPS %.1f below floor %.1f", tps, st.PgbenchTPSFloor)}
	}

	emit(out, "step.restored_load.completed", map[string]any{
		"index":    idx,
		"target":   target,
		"tps":      tps,
		"errors":   errCount,
		"duration": durStr,
		"clients":  clients,
	})
	return StepResult{Index: idx, Kind: st.Kind, Pass: true,
		Message: fmt.Sprintf("restored_load: %.1f TPS, %d errors over %s with %d clients",
			tps, errCount, durStr, clients)}
}

// promoteRestoredSandbox calls pg_promote() in the sandbox and
// polls until pg_is_in_recovery() returns false (max ~30 s).
// Used to flip the sandbox out of recovery mode after the
// restored basebackup + included WAL is replayed — without this
// pgbench's setup phase fails with "cannot execute VACUUM during
// recovery" because the sandbox sits at the default
// recovery_target_action=pause indefinitely.
//
// pg_promote(wait=true) blocks until promotion completes, but
// also has a built-in 60-second timeout — we wrap it in our own
// poll loop with a short overall budget to be safe.  Idempotent:
// if PG is already out of recovery (some future restore path
// might set recovery_target_action=promote), the function
// short-circuits.
func promoteRestoredSandbox(ctx context.Context, sandboxName, sandboxUser string) error {
	already, err := pgIsInRecovery(ctx, sandboxName, sandboxUser)
	if err != nil {
		return fmt.Errorf("check pg_is_in_recovery: %w", err)
	}
	if !already {
		return nil
	}
	// SELECT pg_promote(true, 30) — true: wait for completion;
	// 30: per-call timeout in seconds.  Returns true on
	// successful promotion, false on timeout.
	prom := exec.CommandContext(ctx, "docker", "exec", sandboxName,
		"psql", "-U", sandboxUser, "-At", "-c", "SELECT pg_promote(true, 30)")
	if outBytes, err := prom.CombinedOutput(); err != nil {
		return fmt.Errorf("pg_promote: %v (output: %s)", err, truncate(outBytes, 240))
	}
	// Re-poll for `pg_is_in_recovery() = false` — pg_promote may
	// report success but recovery flag clears asynchronously on
	// older PG majors.
	for i := 0; i < 30; i++ {
		inRec, err := pgIsInRecovery(ctx, sandboxName, sandboxUser)
		if err != nil {
			return fmt.Errorf("poll recovery state: %w", err)
		}
		if !inRec {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("sandbox still in recovery 30s after pg_promote")
}

// pgIsInRecovery returns the boolean value of `pg_is_in_recovery()`
// from the sandbox.
func pgIsInRecovery(ctx context.Context, sandboxName, sandboxUser string) (bool, error) {
	q := exec.CommandContext(ctx, "docker", "exec", sandboxName,
		"psql", "-U", sandboxUser, "-At", "-c", "SELECT pg_is_in_recovery()")
	outBytes, err := q.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("psql: %v (output: %s)", err, truncate(outBytes, 240))
	}
	v := strings.TrimSpace(string(outBytes))
	return v == "t" || v == "true", nil
}

// resolveRestoredTarget returns the datadir to load against,
// in this order: explicit st.Name → captured-state lookup;
// fall back to state.lastRestoreTarget; otherwise refuse.
func resolveRestoredTarget(st scenario.Step, state *runState) (string, error) {
	if name := strings.TrimPrefix(st.Name, "$"); name != "" {
		if state.capturedStates == nil {
			return "", fmt.Errorf("restored_load: name %q referenced but no capture_lsn / restore steps recorded", name)
		}
		cs, ok := state.capturedStates[name]
		if !ok {
			return "", fmt.Errorf("restored_load: name %q not found in captured states (have: %v)", name, sortedCapturedKeys(state.capturedStates))
		}
		if cs.RestoredTarget == "" {
			return "", fmt.Errorf("restored_load: capture %q has no restored target — run a `restore: { to_lsn: $%s }` step before this one", name, name)
		}
		return cs.RestoredTarget, nil
	}
	if state.lastRestoreTarget == "" {
		return "", fmt.Errorf("restored_load: no prior restore step (set name: <captured> referencing a capture_lsn, or insert a `restore:` before this step)")
	}
	return state.lastRestoreTarget, nil
}

// pgPasswordFromDSN pulls the password out of the libpq URI form.
// Returns "" when absent (the test PG image's `trust` auth makes
// password optional from-localhost; we still pass the env var so
// scenarios that DO require auth work).
func pgPasswordFromDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return ""
	}
	pw, _ := u.User.Password()
	return pw
}

// pgDBFromDSN extracts the database name from postgres:// URI's
// path.  Empty path → "" (caller falls back to "postgres").
func pgDBFromDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}

// pgbench summary lines we care about (PG 14-18 stable):
//
//	number of failed transactions: N (...
//	tps = NNN.NNNNNN (without initial connection time)
//
// Both regexes are tight to the canonical form pgbench emits;
// a future format change would surface here as a parseErr,
// which the caller treats as step failure (better than
// silently passing on garbage numbers).
var (
	tpsLineRe    = regexp.MustCompile(`(?m)^tps\s*=\s*([0-9]+(?:\.[0-9]+)?)`)
	failedLineRe = regexp.MustCompile(`(?m)^number of failed transactions:\s*([0-9]+)`)
)

func parsePgbenchOutput(output string) (tps float64, failed int, err error) {
	tm := tpsLineRe.FindStringSubmatch(output)
	if len(tm) < 2 {
		return 0, 0, fmt.Errorf("no `tps =` line found")
	}
	if tps, err = strconv.ParseFloat(tm[1], 64); err != nil {
		return 0, 0, fmt.Errorf("parse TPS %q: %w", tm[1], err)
	}
	// failed line is optional in older pgbench versions; treat
	// missing as 0 failures.
	if fm := failedLineRe.FindStringSubmatch(output); len(fm) >= 2 {
		f, perr := strconv.Atoi(fm[1])
		if perr != nil {
			return tps, 0, fmt.Errorf("parse failed-tx count %q: %w", fm[1], perr)
		}
		failed = f
	}
	return tps, failed, nil
}
