// cli_run step — generic shell-out to the pg_hardstorage binary so
// scenarios can exercise CLI surfaces the runner has no dedicated
// step for.
//
// Why this exists: pg_hardstorage has 40+ top-level commands.  The
// runner has dedicated step kinds only for the data-path ones
// (backup, restore, wal stream, repo init).  Operational-invariant
// commands — hold, kms, audit, recovery, verify, classify, anomaly,
// compliance, threshold, ... — were unit-tested but never E2E-tested
// against a real PG + populated repo.  cli_run is the primitive that
// closes that gap without bloating the step-kind table: scenarios
// drive whatever subcommand they need with literal argv and assert
// on exit code + output substrings.
//
// Placeholder substitution lets a scenario reference scenario state
// without baking in the runner's internal field names — see
// substitutePlaceholders for the mapping.

package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/scenario"
)

// splitPGConn parses a libpq URI ("postgres://user[:pass]@host[:port]/db?...")
// into its host, port, user, and database components.  Empty input
// returns four empty strings — caller's substitutePlaceholders surfaces
// that as the standard "placeholder is empty" error.  Malformed URIs
// also return empties (the placeholder error guides the operator to
// the real problem: PG_CONN not yet populated, not a parse bug).
//
// Lives here rather than in a util package because cli_run is the only
// caller — compat-shim scenarios that need pg1-host/pg1-port/etc as
// individual flags.
func splitPGConn(dsn string) (host, port, user, db string) {
	if dsn == "" {
		return
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return
	}
	host = u.Hostname()
	port = u.Port()
	if u.User != nil {
		user = u.User.Username()
	}
	db = strings.TrimPrefix(u.Path, "/")
	return
}

// runCLIRun executes `pg_hardstorage <args>` and asserts on exit
// code + stdout/stderr substrings.
func runCLIRun(ctx context.Context, st scenario.Step, idx int, state *runState, out io.Writer) StepResult {
	if len(st.Args) == 0 {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: "cli_run: args is required (the argv to pass to pg_hardstorage)"}
	}

	// Auto-init the repo on first cli_run that references $REPO.
	// Scenarios that go straight to a cli_run touching the repo
	// without an earlier take_backup (e.g. L2_threshold_roster,
	// L2_approval_workflow, L1_llm_smoke) would otherwise fail
	// with `notfound.repo: no pg_hardstorage repository at ...`
	// because the runner only auto-inits at take_backup / wal_stream
	// start.  Lazy init here matches operator mental model: "if I'm
	// pointing at $REPO it's expected to exist by now".
	//
	// Exception: when the cli_run itself is `repo init`, skip the
	// auto-init — the scenario is explicitly exercising repo init
	// and a pre-emptive init would surface as conflict.repo_exists
	// from the scenario's own `repo init` step (a regression
	// L1_cli_run_smoke turned up).
	if !state.repoInited {
		isRepoInit := len(st.Args) >= 2 && st.Args[0] == "repo" && st.Args[1] == "init"
		if !isRepoInit {
			for _, a := range st.Args {
				if strings.Contains(a, "$REPO") {
					if err := initRepo(ctx, state, out); err != nil {
						return StepResult{Index: idx, Kind: st.Kind, Pass: false,
							Message: fmt.Sprintf("cli_run: auto repo init: %v", err)}
					}
					state.repoInited = true
					break
				}
			}
		}
	}

	// Resolve placeholders in argv.  Empty placeholder values
	// surface as a step-fail with a clear pointer; this is
	// almost always a scenario bug (e.g. referencing
	// $LAST_BACKUP before any take_backup step ran).
	resolved := make([]string, len(st.Args))
	for i, raw := range st.Args {
		v, err := substitutePlaceholders(raw, state)
		if err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("cli_run: arg[%d] %q: %v", i, raw, err)}
		}
		resolved[i] = v
	}

	timeout := 60 * time.Second
	if s := strings.TrimSpace(st.Timeout); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("cli_run: parse timeout %q: %v", s, err)}
		}
		if d <= 0 {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: "cli_run: timeout must be > 0"}
		}
		timeout = d
	}

	expectExit := 0
	if st.ExpectExit != nil {
		expectExit = *st.ExpectExit
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Pick the binary to exec.  Default = the native agent
	// (state.agentBin).  When `shim:` is set, we shell out to
	// the compat shim of that name instead — used by scenarios
	// exercising the migration shims' CLI behaviour (e.g.
	// `barman backup --reuse-backup=link` honesty-checks).
	// resolveShimBinary handles env-var override
	// (PG_HARDSTORAGE_<NAME>_BIN), `./bin/pg-hardstorage-<name>`,
	// then `pg-hardstorage-<name>` on PATH — same precedence as
	// compat_archive uses, so a scenario that mixes cli_run
	// and compat_archive against the same shim picks the same
	// binary in both.
	binary := state.agentBin
	if strings.TrimSpace(st.Shim) != "" {
		shimBin, err := resolveShimBinary(st.Shim)
		if err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("cli_run: shim %q: %v", st.Shim, err)}
		}
		binary = shimBin
	}
	cmd := exec.CommandContext(cctx, binary, resolved...)

	// Plumb the agent env (sink credentials, AWS_CA_BUNDLE,
	// PG_HARDSTORAGE_LLM_*, ...) into the child so cli_run
	// reaches whatever backend the scenario configured.  Then
	// overlay any step-scoped env: from the YAML — placeholders
	// resolved, st.Env values WIN over agentEnv (the scenario
	// is being deliberately explicit).  Compat-shim scenarios
	// use this for PGPASSWORD; future operator-flow scenarios
	// might use it for PG_HARDSTORAGE_AIRGAPPED or similar.
	if len(state.agentEnv) > 0 || len(st.Env) > 0 {
		base := cmd.Environ()
		merged := make(map[string]string, len(state.agentEnv)+len(st.Env))
		for k, v := range state.agentEnv {
			merged[k] = v
		}
		for k, v := range st.Env {
			rv, err := substitutePlaceholders(v, state)
			if err != nil {
				return StepResult{Index: idx, Kind: st.Kind, Pass: false,
					Message: fmt.Sprintf("cli_run: env[%s]: %v", k, err)}
			}
			merged[k] = rv
		}
		cmd.Env = appendEnv(base, merged)
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	emit(out, "step.cli_run.starting", map[string]any{
		"index":   idx,
		"args":    resolved,
		"timeout": timeout.String(),
	})

	runErr := cmd.Run()

	// exec.Run returns *exec.ExitError for non-zero exits;
	// use ExitCode() to pull the numeric status.  -1 from
	// ExitCode() means "not exited normally" (signal,
	// timeout) — surface as a step fail with the wrapped
	// error message rather than a misleading "exit -1".
	gotExit := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			gotExit = ee.ExitCode()
		} else {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("cli_run: %v (stderr: %s)", runErr,
					truncate(stderrBuf.Bytes(), 256))}
		}
	}

	if gotExit != expectExit {
		return StepResult{Index: idx, Kind: st.Kind, Pass: false,
			Message: fmt.Sprintf("cli_run: exit %d, want %d (stderr: %s)",
				gotExit, expectExit, truncate(stderrBuf.Bytes(), 256))}
	}

	// expect_*_contains accept the same $PLACEHOLDER syntax
	// the args do, so a scenario can assert "stdout contains
	// $LAST_BACKUP" without needing to bake the backup ID
	// into the YAML literally.
	if raw := st.ExpectStdoutContains; raw != "" {
		want, err := substitutePlaceholders(raw, state)
		if err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("cli_run: expect_stdout_contains %q: %v", raw, err)}
		}
		if !strings.Contains(stdoutBuf.String(), want) {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("cli_run: stdout missing %q (stdout: %s)",
					want, truncate(stdoutBuf.Bytes(), 256))}
		}
	}
	if raw := st.ExpectStderrContains; raw != "" {
		want, err := substitutePlaceholders(raw, state)
		if err != nil {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("cli_run: expect_stderr_contains %q: %v", raw, err)}
		}
		if !strings.Contains(stderrBuf.String(), want) {
			return StepResult{Index: idx, Kind: st.Kind, Pass: false,
				Message: fmt.Sprintf("cli_run: stderr missing %q (stderr: %s)",
					want, truncate(stderrBuf.Bytes(), 256))}
		}
	}

	emit(out, "step.cli_run.completed", map[string]any{
		"index":  idx,
		"args":   resolved,
		"exit":   gotExit,
		"stdout": truncate(stdoutBuf.Bytes(), 256),
	})
	return StepResult{Index: idx, Kind: st.Kind, Pass: true,
		Message: fmt.Sprintf("cli_run: %v exit=%d", resolved, gotExit)}
}

// substitutePlaceholders expands $NAME tokens in s against runState.
// Unknown placeholders are an error (catches typos like $LASTBACKUP
// before they masquerade as literal flag values).  Empty resolved
// values are also an error (catches $LAST_BACKUP referenced before
// any take_backup step ran).
//
// Supported names:
//
//	$REPO          → state.repoURL
//	$DEPLOYMENT    → state.deployment
//	$LAST_BACKUP   → state.lastBackup
//	$ARTEFACT_DIR  → state.artefactDir
//	$AGENT_BIN     → state.agentBin
//	$PG_CONN       → topology's CURRENT-leader DSN
//	$BACKUP_<name> → state.namedBackups[<name>] (set by take_backup
//	                 steps with name:).  E.g. $BACKUP_pre.
//
// We intentionally don't support generic env-var expansion: a
// scenario that needs an env var should set it via the runner
// invocation and the child inherits.  Limiting placeholders to
// runState fields keeps the YAML-to-runtime contract small.
func substitutePlaceholders(s string, state *runState) (string, error) {
	if !strings.Contains(s, "$") {
		return s, nil
	}
	type binding struct {
		name string
		val  string
	}
	// connStringSafe wraps state.connString to tolerate the
	// nil-callback case our unit tests use (they instantiate
	// runState by hand).  Production callers always set the
	// callback at scenario start.
	connStringSafe := func(s *runState) string {
		if s == nil || s.connString == nil {
			return ""
		}
		return s.connString()
	}
	// Parse $PG_CONN into component fields so scenarios that
	// shell out to compat shims (pgbackrest, walg, barman) can
	// use the structured flags (--pg1-host/port/user/database in
	// pgBackRest's case) without re-implementing DSN parsing per
	// scenario.  An empty $PG_CONN leaves all fields empty,
	// which surfaces as the same "placeholder is empty" error
	// the rest of the bindings produce.
	pgHost, pgPort, pgUser, pgDB := splitPGConn(connStringSafe(state))
	bindings := []binding{
		{"$REPO", state.repoURL},
		{"$DEPLOYMENT", state.deployment},
		{"$LAST_BACKUP", state.lastBackup},
		{"$ARTEFACT_DIR", state.artefactDir},
		{"$AGENT_BIN", state.agentBin},
		// $PG_CONN — the topology's CURRENT-leader DSN (re-resolved
		// every cli_run dispatch so a post-failover step still
		// targets the writable primary).  L4 upgrade/compat
		// scenarios that shell out to `pg_hardstorage backup` /
		// `restore` directly need this; without it the CLI errors
		// `usage.missing_flag: --pg-connection is required` before
		// the test reaches its actual assertion target.
		{"$PG_CONN", connStringSafe(state)},
		{"$PG_HOST", pgHost},
		{"$PG_PORT", pgPort},
		{"$PG_USER", pgUser},
		{"$PG_DB", pgDB},
	}
	out := s
	for _, b := range bindings {
		if !strings.Contains(out, b.name) {
			continue
		}
		if b.val == "" {
			return "", fmt.Errorf("placeholder %s is empty (likely referenced before the prerequisite step ran — e.g. $LAST_BACKUP before any take_backup)", b.name)
		}
		out = strings.ReplaceAll(out, b.name, b.val)
	}
	// $BACKUP_<name> — pattern-based binding for named backups.
	// Searches for $BACKUP_ followed by an identifier and resolves
	// against state.namedBackups.  An unknown name is an error so
	// typos (e.g. $BACKUP_pretroyation) surface at scenario time
	// instead of as a literal flag value at exec time.
	if strings.Contains(out, "$BACKUP_") {
		out = expandNamedBackupRefs(out, state)
		// expandNamedBackupRefs returns the input with an
		// "$<UNRESOLVED:...>" marker for unknown names — the
		// final $TOKEN check below catches them.
	}
	// Reject unresolved $TOKEN — likely a typo.  Skip the
	// dollar-prefix check for shell-style "$1" (positional
	// args don't appear in our placeholder set; literal "$"
	// without an alpha follower is fine).
	if i := strings.Index(out, "$"); i >= 0 && i+1 < len(out) {
		next := out[i+1]
		if (next >= 'A' && next <= 'Z') || next == '_' {
			return "", fmt.Errorf("unrecognised placeholder near %q (known: $REPO $DEPLOYMENT $LAST_BACKUP $ARTEFACT_DIR $AGENT_BIN $PG_CONN $PG_HOST $PG_PORT $PG_USER $PG_DB $BACKUP_<name>)", out[i:])
		}
	}
	return out, nil
}

// expandNamedBackupRefs replaces every $BACKUP_<name> in s with
// state.namedBackups[<name>], or leaves an "$<UNRESOLVED:...>"
// marker that the caller's unknown-placeholder check converts
// into a clear scenario error.  <name> matches [A-Za-z0-9_]+.
func expandNamedBackupRefs(s string, state *runState) string {
	const prefix = "$BACKUP_"
	var b strings.Builder
	for {
		i := strings.Index(s, prefix)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		// Consume the identifier following the prefix.
		j := i + len(prefix)
		for j < len(s) {
			c := s[j]
			if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
				break
			}
			j++
		}
		name := s[i+len(prefix) : j]
		if name == "" {
			// Bare "$BACKUP_" with no identifier — leave it so
			// the unresolved-$TOKEN check below errors.
			b.WriteString(prefix)
		} else if v, ok := state.namedBackups[name]; ok && v != "" {
			b.WriteString(v)
		} else {
			b.WriteString("$UNRESOLVED_BACKUP_" + name)
		}
		s = s[j:]
	}
}

// appendEnv overlays kvs onto base, returning a new slice in the
// "KEY=VALUE" form os/exec wants.  Later definitions of the same
// key win — agentEnv overrides anything in os.Environ.
func appendEnv(base []string, kvs map[string]string) []string {
	have := make(map[string]bool, len(kvs))
	out := make([]string, 0, len(base)+len(kvs))
	for _, e := range base {
		eq := strings.IndexByte(e, '=')
		if eq > 0 {
			k := e[:eq]
			if _, override := kvs[k]; override {
				continue
			}
		}
		out = append(out, e)
	}
	for k, v := range kvs {
		out = append(out, k+"="+v)
		have[k] = true
	}
	return out
}
