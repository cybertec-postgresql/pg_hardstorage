// Package postverify is the post-restore "does the cluster
// actually start and respond?" smoke test — defence layer L3
// in the restore-correctness stack.
//
// What it catches that L1/L2 miss
// -------------------------------
// L1 (Manifest.Validate) and L2 (in-process pg_verifybackup)
// only see what's IN the manifest.  They cannot detect:
//
//   - empty PG-required dirs missing — issue #7's class:
//     pg_wal/, pg_dynshmem/, etc. don't appear in PG's
//     backup_manifest, so L2 has nothing to check.  PG
//     refuses to start, L3 catches this.
//   - permissions broken on PGDATA root or subtree — PG
//     will refuse to start with "permissions on data
//     directory are too liberal" or similar.
//   - tablespace symlinks unrebuilt or pointing nowhere —
//     PG's startup code chases them and errors out.
//   - postgresql.conf / pg_hba.conf absent or malformed —
//     PG won't start.
//   - silent WAL replay corruption that prevents a clean
//     "ready for connections" — postmaster's recovery
//     loop logs the error.
//
// In short: this is the layer that proves the restored
// datadir is a USABLE PG cluster, not just a byte-equal
// reconstitution of files.  Without it the operator
// discovers issue-#7-class regressions only when they try
// to start the cluster — at which point the backup window
// has already closed.
//
// Modes
// -----
//
//	Off       — skip entirely; legacy behaviour, escape hatch.
//	Auto      — try host pg_ctl; soft-skip with WARN if absent.
//	Required  — hard-fail if no environment is available.
//	Dump      — Auto + run pg_dumpall against the started cluster
//	            (L4 territory; planned).
//
// Default: Auto.  Operators who don't have PG tooling on the
// runner host get a loud WARN explaining why the gate didn't
// run; CI environments that mandate the gate flip to Required.
package postverify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/walfetchcmd"
)

// Mode controls how aggressive the post-restore verification
// is.  See package docstring for semantics.
type Mode string

const (
	// ModeOff disables the post-restore gate entirely. Legacy
	// escape hatch; not recommended outside diagnostic workflows.
	ModeOff Mode = "off"

	// ModeAuto tries the host pg_ctl. If PG tooling is absent the
	// gate soft-skips with a loud WARN explaining why it didn't
	// run. Default.
	ModeAuto Mode = "auto"

	// ModeRequired hard-fails the restore if no PG environment is
	// available to run the smoke test. Used by CI / regulated
	// fleets that mandate the gate.
	ModeRequired Mode = "required"

	// ModeDump is ModeAuto plus a pg_dumpall round-trip against
	// the started cluster — defence layer L4. Planned; behaves
	// like ModeAuto until the dump runner lands.
	ModeDump Mode = "dump"
)

// probeWaitDelay bounds how long a probe's Wait may block on I/O
// after its context is cancelled.  exec.CommandContext kills only
// the direct child (the `sh`/`psql` process); a grandchild that
// inherited the stdout/stderr pipe (e.g. a `sleep` spawned by a
// wrapper script, or psql's own libpq connect retry) keeps the
// pipe's write end open, so CombinedOutput would otherwise block
// until that grandchild exits — defeating ctx cancellation.
// Setting cmd.WaitDelay force-closes the pipes this long after the
// kill, letting Wait return promptly.  Normal (uncancelled) runs
// are unaffected: WaitDelay only applies once the process has been
// killed or has already exited with I/O still open.
const probeWaitDelay = 250 * time.Millisecond

// ParseMode normalises an operator-supplied string into a
// Mode constant.  Empty defaults to Auto.  Unknown values
// fail loudly so a typo in --verify-restore doesn't
// silently downgrade to "off".
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return ModeAuto, nil
	// "skip" is accepted as an alias so the two sibling restore gates
	// (--verify skip|require, --verify-restore off|required) don't
	// punish operators for guessing the other flag's vocabulary.
	case "off", "false", "no", "none", "skip":
		return ModeOff, nil
	case "required", "strict", "require", "yes":
		return ModeRequired, nil
	case "dump":
		return ModeDump, nil
	}
	return "", fmt.Errorf("postverify: unknown mode %q (want off|auto|required|dump)", s)
}

// Options drive a single Verify call.
type Options struct {
	Mode Mode

	// DataDir is the restored PGDATA to start.
	DataDir string

	// PGMajorVersion is the major version the manifest
	// claims (e.g. 17, 18).  Used to pick a matching pg_ctl
	// when multiple PG installs are available, and for the
	// future Docker-fallback image tag.
	PGMajorVersion int

	// PGUser is the OS user the cluster was initdb'd under.
	// Default: "postgres".  When pg_ctl is run by the same
	// user that owns DataDir, this isn't needed; it's an
	// override for unusual setups.
	PGUser string

	// StartTimeout caps how long we wait for postmaster to
	// reach "ready for connections".  Default 60s; for
	// large restored clusters with crash recovery to do at
	// startup, raise via the CLI flag.
	StartTimeout time.Duration

	// RecoveryArmed indicates the restore wrote
	// recovery.signal / standby.signal.  In that case PG
	// enters recovery mode at startup; postverify uses a
	// MINIMAL probe (start + pg_isready) rather than
	// running catalog queries which may not be available
	// during recovery.
	RecoveryArmed bool

	// RepoURL + Deployment, when both non-empty, drive
	// stageForRecovery to wire `restore_command =
	// '<agent> wal fetch <Deployment> %f %p --repo <RepoURL>'`
	// into postgresql.auto.conf.  Without this, PG enters
	// standby and sits waiting for the basebackup STOP-LSN
	// segment to appear in pg_wal/ — but `pg_hardstorage
	// backup` doesn't bundle the trailing WAL into the
	// restored datadir, so the wait is forever and
	// postverify times out.  The fix surfaced as the
	// dominant failure cluster in soak testing (10 of 16
	// soak cells + test-wal-stream-suite/assert_restored_match).
	RepoURL    string
	Deployment string

	// AgentBinary, when non-empty, overrides the binary path
	// used in the synthesised restore_command.  Defaults to
	// os.Executable() (the agent invoking postverify).
	// Tests use this to point at a stub binary.
	AgentBinary string
}

// Result summarises what the verifier did.  Returned even on
// error so audit logs capture progress.
type Result struct {
	Mode          Mode
	Path          string // path used to find pg_ctl, if any
	StartDuration time.Duration
	QueriesRan    int
	DumpRan       bool          // true iff Mode=Dump completed pg_dumpall
	DumpDuration  time.Duration // 0 unless DumpRan
	Skipped       bool
	SkipReason    string

	// ProbeDSNKind records which connection mode the probe used:
	// "socket" (peer auth via the Unix socket pg_ctl placed in
	// DataDir, preferred — no password) or "tcp" (127.0.0.1
	// fallback used when the socket attempt failed).  Issue #85.
	ProbeDSNKind string
}

// ErrNoEnvironment is returned by Verify when Mode=Required
// and no usable PG environment (host pg_ctl) was found.
var ErrNoEnvironment = errors.New("postverify: no PG runtime available (install postgresql-client/server or set Mode=off)")

// Verify runs the post-restore smoke test according to opts.
// Returns nil on success or when soft-skipping (Mode=Auto +
// no PG); returns an error on hard-fail.  Always returns a
// non-nil *Result (even on error) so callers can record what
// was attempted in the audit log.
func Verify(ctx context.Context, opts Options) (*Result, error) {
	if opts.Mode == "" {
		opts.Mode = ModeAuto
	}
	if opts.PGUser == "" {
		opts.PGUser = "postgres"
	}
	if opts.StartTimeout <= 0 {
		// 180s (was 60s) because a freshly-restored cluster
		// recovering from a fault-injected source can spend
		// 10-30s on fsync of the data dir + 10-60s walking the
		// embedded WAL slice before recovery_target=immediate
		// fires.  A soak run's iter=25 verify on a kill -9'd
		// cell consistently hit `pg_ctl: server did not start in
		// time` at 60s after only reaching "entering standby
		// mode" — bumping to 180s clears the window without
		// making no-fault verifies visibly slower (pg_ctl -w
		// returns the moment PG accepts connections, not at
		// timeout).
		opts.StartTimeout = 180 * time.Second
	}

	res := &Result{Mode: opts.Mode}

	if opts.Mode == ModeOff {
		res.Skipped = true
		res.SkipReason = "Mode=off"
		return res, nil
	}

	// Sanity: a real PGDATA always has global/pg_control.
	// Test fixtures and partial-restore previews don't.  In
	// Auto mode we silently skip on absent pg_control rather
	// than firing pg_ctl against a directory that isn't a
	// cluster (which would fail with a misleading error).
	// Required mode hard-fails — operators who set Required
	// expect a real backup target.
	if _, err := os.Stat(filepath.Join(opts.DataDir, "global", "pg_control")); err != nil {
		switch opts.Mode {
		case ModeRequired:
			return res, fmt.Errorf("postverify: target lacks global/pg_control (not a valid PGDATA): %w", err)
		default:
			res.Skipped = true
			res.SkipReason = "target lacks global/pg_control (not a real PGDATA — fixture or preview?)"
			return res, nil
		}
	}

	// Locate pg_ctl + psql on the host.  Probe multiple
	// install paths since PGDG-RPM uses /usr/pgsql-N/bin,
	// PGDG-APT uses /usr/lib/postgresql/N/bin, distros use
	// /usr/bin via wrapper scripts.
	binDir, err := findHostPGBin(opts.PGMajorVersion)
	if err != nil {
		switch opts.Mode {
		case ModeRequired:
			return res, ErrNoEnvironment
		default:
			res.Skipped = true
			res.SkipReason = err.Error()
			return res, nil
		}
	}
	res.Path = binDir

	// Pick a free localhost port.  Listen on :0 with the
	// kernel handing us a port, then close — the chance of
	// collision before PG binds is vanishingly small for a
	// few-second restore-verify window.
	port, err := pickFreePort()
	if err != nil {
		return res, fmt.Errorf("postverify: pick port: %w", err)
	}

	pgCtl := filepath.Join(binDir, "pg_ctl")
	psql := filepath.Join(binDir, "psql")
	logFile := filepath.Join(opts.DataDir, "postverify-postgres.log")

	// Hard pre-flight: PG refuses to start when launched as root
	// ("pg_ctl: cannot be run as root").  The top-level CLI gate
	// (internal/cli/refuse_root.go) blocks euid 0 long before we
	// get here, so the only way this branch fires is a caller
	// embedding postverify directly and bypassing the gate.
	// Refuse loudly rather than try to drop privileges — the
	// drop-priv path was the source of multiple historical bugs
	// (chown races, repo-readability hacks, pgbackup-vs-postgres
	// confusion).  Operators should rebuild their container /
	// service to run as a non-root system user (pgbackup, or any
	// equivalent).
	if os.Geteuid() == 0 {
		return res, fmt.Errorf("postverify: refuses to run as root (euid 0); rebuild the container to run as a non-root system user — see deploy/systemd/pg_hardstorage.service (User=pgbackup)")
	}

	// Stage the data dir for archive recovery before pg_ctl
	// start.  Why: a pg_hardstorage backup snapshots PGDATA
	// while the source cluster is live, so the restored
	// pg_control records a checkpoint LSN that lives in a WAL
	// segment not present in pg_wal/.  Without an explicit
	// recovery.signal, PG opens the cluster as if it had
	// crashed and tries to redo from that exact LSN — and
	// fails immediately:
	//
	//   FATAL: could not locate required checkpoint record at 0/A000080
	//   HINT: If you are restoring from a backup, touch
	//   ".../recovery.signal" or ".../standby.signal"
	//
	// `recovery.signal` puts PG in archive-recovery mode;
	// `recovery_target = 'immediate'` tells it to stop at
	// the first consistency point reached during WAL replay
	// (i.e. the end of the basebackup's embedded WAL slice)
	// and promote.  Without `recovery_target=immediate`,
	// archive recovery would loop forever waiting on a
	// restore_command we don't ship.  `recovery_target_action
	// = 'promote'` makes the cluster open RW after stopping,
	// which the smoke probes below need.
	//
	// Reproduced across PG 15 / 16 / 17 / 18 in the soak testing
	// soak — every postverify hit "could not locate required
	// checkpoint record" until this staging block went in.
	//
	// EXCEPTION (opts.RecoveryArmed): a PITR restore already wrote an
	// explicit recovery_target_* block + recovery.signal, so
	// stageForRecovery only wires the missing restore_command and skips
	// the recovery_target='immediate' line — two targets would FATAL
	// with "multiple recovery targets specified" (issue #56).
	if err := stageForRecovery(opts.DataDir, opts.RepoURL, opts.Deployment, opts.AgentBinary, opts.RecoveryArmed); err != nil {
		return res, fmt.Errorf("postverify: stage for recovery: %w", err)
	}

	// Start.  -w blocks until "ready for connections" or
	// timeout; -t maps to that timeout.  The -o options give
	// us a sandbox port + localhost-only listening so we
	// can't accidentally expose the verifier's PG to the
	// network.
	//
	// unix_socket_directories is pinned to the data dir so the
	// verifier's postmaster never tries to write to the
	// distro default (/var/run/postgresql on Debian/Ubuntu's
	// pgdg build), which is owned by the postgres system user
	// and unwritable for the postverify caller in unprivileged
	// CI runners.  This was the dominant failure mode on the
	// docs-doctest job after the pg-major-pin landed: PG
	// started, found a default unix-socket path it couldn't
	// write to, and aborted with "could not create lock file
	// /var/run/postgresql/.s.PGSQL.NNNN.lock: Permission
	// denied".
	// max_connections is deliberately NOT overridden. PostgreSQL
	// archive recovery aborts at startup —
	//
	//   FATAL: recovery aborted because of insufficient parameter
	//   settings
	//   DETAIL: max_connections = 10 is a lower setting than on the
	//   primary server, where its value was 100.
	//
	// — whenever the recovering server's max_connections (or
	// max_worker_processes / max_wal_senders /
	// max_prepared_transactions / max_locks_per_transaction) is
	// LOWER than the value the source cluster ran with: those GUCs
	// size shared-memory structures the WAL replay depends on. An
	// earlier `-c max_connections=10`, added to keep the verifier
	// lightweight, forced it below the source's default 100 and
	// broke restore-verify of every normally-configured cluster
	// (GH #43). Like the four sibling recovery parameters — which
	// postverify already leaves alone — max_connections now
	// inherits the restored postgresql.conf, which BASE_BACKUP
	// captures from the source, so it always satisfies the
	// recovery floor. shared_buffers stays pinned: it has no
	// recovery floor and is the real memory knob.
	pgOpts := fmt.Sprintf("-p %d -c listen_addresses=127.0.0.1 -c unix_socket_directories=%s -c logging_collector=off -c shared_buffers=128MB",
		port, opts.DataDir)
	startArgs := []string{
		"-D", opts.DataDir,
		"-l", logFile,
		"-o", pgOpts,
		"-t", fmt.Sprintf("%d", int(opts.StartTimeout.Seconds())),
		"-w", "start",
	}
	// Always stop on the way out, even when pg_ctl start itself
	// FAILS.  `pg_ctl -w start` can report failure (non-zero exit,
	// e.g. "server did not start in time") while the postmaster it
	// forked is in fact alive and holding the datadir lockfile +
	// port — a leaked live postmaster.  registerStopGuard installs
	// the best-effort `pg_ctl stop` defer BEFORE the start attempt
	// (rather than after it succeeds) so the cleanup fires on the
	// failure path too.
	defer registerStopGuard(pgCtl, opts.DataDir)()

	startedAt := time.Now()
	if out, err := runStart(ctx, pgCtl, startArgs); err != nil {
		// Capture the postgres start log for forensics.  Truncation
		// limits were originally 256 / 1024 but a tier-7 validate
		// soak surfaced a B11-class failure whose actual FATAL log
		// line sat past the 1024-byte truncation point, leaving
		// operators (and post-mortem investigation) with only the
		// preamble of wal-fetch probe lines and no root cause.
		// 4096 / 8192 lets the fatal pg_ctl reason ("could not
		// locate required checkpoint record at …", "data directory
		// has invalid permissions", "database files are
		// incompatible with server", etc.) actually appear.  Costs
		// a few KB per failing restore; harmless on the success
		// path.
		logTail := readTail(logFile, 16384)
		return res, fmt.Errorf("postverify: pg_ctl start: %w (output: %s; log tail: %s)",
			err, truncate(out, 4096), truncate([]byte(logTail), 8192))
	}
	res.StartDuration = time.Since(startedAt)

	// (the stop guard was registered before the start attempt above
	// so it also cleans up a postmaster leaked by a failed start.)

	// Connection-string preference (issue #85): the local Unix
	// socket has two advantages over TCP for the postverify
	// probe:
	//
	//   - Peer authentication is the stock pg_hba.conf default
	//     for `local` connections.  PG looks at the OS uid that
	//     opened the socket and authenticates it directly — no
	//     password needed.  TCP from 127.0.0.1 falls into the
	//     `host` `pg_hba.conf` block whose default is `scram-
	//     sha-256` / `password`, so the operator gets prompted.
	//
	//   - pg_ctl's `-c unix_socket_directories=<DataDir>` (set
	//     above) places the socket at a path we own, so there's
	//     no ambiguity about which postmaster we're talking to.
	//
	// We try the socket first, fall back to TCP if it errors —
	// matches the reporter's expectation in #85.  Whichever DSN
	// we use, psql gets `-w` (`--no-password`) so a failed auth
	// errors out immediately instead of prompting the operator
	// for input that would never arrive in an automated restore.
	socketDSN := fmt.Sprintf("postgres:///postgres?host=%s&port=%d&user=%s&sslmode=disable",
		url.QueryEscape(opts.DataDir), port, url.QueryEscape(opts.PGUser))
	tcpDSN := fmt.Sprintf("postgres://%s@127.0.0.1:%d/postgres?sslmode=disable", opts.PGUser, port)

	// pickProbeDSN tries the socket DSN against probeSelect1;
	// the first probe that succeeds picks the DSN every later
	// step in this function uses.  If both fail, both error
	// messages are surfaced so the operator can act on whichever
	// one applies.
	dsn, dsnKind, err := pickProbeDSN(ctx, psql, socketDSN, tcpDSN)
	if err != nil {
		return res, fmt.Errorf("postverify: SELECT 1: %w", err)
	}
	res.QueriesRan++
	res.ProbeDSNKind = dsnKind

	if !opts.RecoveryArmed {
		if err := probeCatalogs(ctx, psql, dsn); err != nil {
			return res, fmt.Errorf("postverify: catalog probe: %w", err)
		}
		res.QueriesRan++
	}

	// L4 — pg_dumpall to /dev/null.  Reads every relation
	// in every database, dereferencing every row.  Catches
	// corruption that no other layer can see: torn pages,
	// broken visibility maps, FSM/VM inconsistency, missing
	// row data behind valid catalog entries.  Time grows
	// with DB size — a few seconds for a smoke DB, minutes
	// for a 10 GB DB, hours for a 1 TB DB.  Operators who
	// don't want the wait stay on Mode=Auto; CI on
	// representative-size DBs sets Mode=Dump.
	//
	// Skipped when RecoveryArmed: pg_dump can't safely run
	// against a cluster paused mid-recovery, and a promote
	// action puts the cluster on a new timeline that diverges
	// from the source — neither matches the "byte-equal
	// source" semantics dump-verification implies.
	if opts.Mode == ModeDump && !opts.RecoveryArmed {
		dumpStart := time.Now()
		if err := probeDumpall(ctx, binDir, dsn); err != nil {
			return res, fmt.Errorf("postverify: pg_dumpall: %w", err)
		}
		res.DumpRan = true
		res.DumpDuration = time.Since(dumpStart)
	}

	return res, nil
}

// runStart runs `pg_ctl … -w start` and returns its combined
// output + error.  Split out from Verify so the start/stop
// sequencing (the stop guard must be armed BEFORE this runs) is
// unit-testable with a stub pg_ctl — see bug 52.
func runStart(ctx context.Context, pgCtl string, startArgs []string) ([]byte, error) {
	startCmd := exec.CommandContext(ctx, pgCtl, startArgs...)
	return startCmd.CombinedOutput()
}

// registerStopGuard returns a best-effort `pg_ctl -m fast stop`
// closure for dataDir.  Callers `defer` the returned func BEFORE
// attempting start so a postmaster leaked by a FAILED
// `pg_ctl -w start` (non-zero exit but a live forked postmaster
// still holding the datadir lock + port) is still stopped.  A
// stop against a stopped/absent postmaster is a harmless no-op
// whose error is intentionally discarded; a hung postmaster is
// bounded to 30s so it can't block the caller.
func registerStopGuard(pgCtl, dataDir string) func() {
	return func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		stopCmd := exec.CommandContext(stopCtx, pgCtl,
			"-D", dataDir, "-m", "fast", "stop")
		_ = stopCmd.Run()
	}
}

// probeDumpall runs `pg_dumpall > /dev/null` against the
// sandbox cluster.  Output is discarded — we only care that
// the dump COMPLETES without errors, which means PG could
// read every relation in every (non-template) database
// without hitting corruption.
//
// Why pg_dumpall (not pg_dump per database):
//   - single command covers all databases including roles
//     and tablespace metadata, simpler to wire
//   - exits non-zero on any per-database failure, surfacing
//     the first corruption hit
//
// Schema-only mode is intentionally NOT used here — that
// would skip data pages, leaving torn-page corruption
// undetected.  Operators wanting a fast gate keep Mode=Auto;
// Mode=Dump is the slow comprehensive gate.
func probeDumpall(ctx context.Context, binDir, dsn string) error {
	bin := filepath.Join(binDir, "pg_dumpall")
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("pg_dumpall not found in %s: %w", binDir, err)
	}
	// `-w` (--no-password) mirrors the psql probes' no-password
	// posture from issue #85.  A failed auth errors out cleanly
	// instead of stalling on a prompt automation can't satisfy.
	cmd := exec.CommandContext(ctx, bin,
		"-w",
		"-d", dsn,
		"-f", os.DevNull,
	)
	cmd.WaitDelay = probeWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w (output: %s)", err, truncate(out, 512))
	}
	return nil
}

// findHostPGBin walks the standard PG install paths looking
// for pg_ctl.  Prefers a major-version match when one is
// requested (e.g. major=17 → /usr/lib/postgresql/17/bin,
// /usr/pgsql-17/bin) before falling back to whatever pg_ctl
// is on PATH.
//
// Returns the directory of pg_ctl, or an error if no
// candidate was found.
func findHostPGBin(major int) (string, error) {
	if major > 0 {
		// Versioned install dirs differ by distro family:
		//   Debian / PGDG-APT → /usr/lib/postgresql/<major>/bin
		//   PGDG-RPM (RHEL)   → /usr/pgsql-<major>/bin
		//   openSUSE          → /usr/lib/postgresql<major>/bin
		majorPaths := []string{
			fmt.Sprintf("/usr/lib/postgresql/%d/bin", major),
			fmt.Sprintf("/usr/pgsql-%d/bin", major),
			fmt.Sprintf("/usr/lib/postgresql%d/bin", major),
		}
		for _, p := range majorPaths {
			if _, err := os.Stat(filepath.Join(p, "pg_ctl")); err == nil {
				return p, nil
			}
		}
		// Distros whose package installs a single PG major
		// straight onto PATH instead of a versioned dir —
		// Arch's `postgresql`, Fedora's `postgresql-server`.
		// Accept an on-PATH pg_ctl too, but ONLY when its
		// reported major matches: running a mismatched major
		// against the restored datadir surfaces confusingly
		// as "database files are incompatible with server".
		for _, p := range []string{"/usr/bin", "/usr/local/bin"} {
			cand := filepath.Join(p, "pg_ctl")
			if _, err := os.Stat(cand); err == nil && pgCtlMajor(cand) == major {
				return p, nil
			}
		}
		if p, err := exec.LookPath("pg_ctl"); err == nil && pgCtlMajor(p) == major {
			return filepath.Dir(p), nil
		}
		// Major-pinned but no matching binary on this host.
		// Refuse to fall back to a different major — pg_ctl
		// against a mismatched datadir surfaces as "database
		// files are incompatible with server", which gets
		// reported to the operator as a verify failure when
		// the real outcome is "this host can't verify a
		// backup of this major".  Returning a clear
		// not-found error lets ModeAuto callers soft-skip
		// with a useful reason instead.
		return "", fmt.Errorf("pg_ctl for PG major %d not found: probed /usr/lib/postgresql/%d/bin, /usr/pgsql-%d/bin, /usr/lib/postgresql%d/bin and a major-matched /usr/bin install (install postgresql-%d to enable postverify on this manifest)",
			major, major, major, major, major)
	}
	// major == 0 — manifest didn't record a version, or caller
	// didn't pass it.  Accept any installed major.
	candidates := []string{
		"/usr/lib/postgresql/*/bin", // Debian / PGDG-APT
		"/usr/lib/postgresql*/bin",  // openSUSE (unslashed major)
		"/usr/pgsql-*/bin",          // PGDG-RPM
		"/opt/homebrew/opt/postgresql/bin",
	}
	for _, pat := range candidates {
		matches, _ := filepath.Glob(pat)
		if len(matches) == 0 {
			if _, err := os.Stat(filepath.Join(pat, "pg_ctl")); err == nil {
				return pat, nil
			}
			continue
		}
		for _, m := range matches {
			if _, err := os.Stat(filepath.Join(m, "pg_ctl")); err == nil {
				return m, nil
			}
		}
	}
	if p, err := exec.LookPath("pg_ctl"); err == nil {
		return filepath.Dir(p), nil
	}
	return "", fmt.Errorf("pg_ctl not found in standard locations or on PATH")
}

// pgCtlMajor runs `<pgCtl> --version` and returns the major
// version, or 0 when it cannot be determined.  `pg_ctl
// --version` prints e.g. "pg_ctl (PostgreSQL) 18.1" on a
// release build or "pg_ctl (PostgreSQL) 18beta1" on a dev
// build — the major is the leading integer of the last field.
func pgCtlMajor(pgCtl string) int {
	out, err := exec.Command(pgCtl, "--version").Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0
	}
	var major int
	if _, err := fmt.Sscanf(fields[len(fields)-1], "%d", &major); err != nil {
		return 0
	}
	return major
}

// pickFreePort asks the kernel for an unused localhost port.
// We close the listener immediately; the brief race window
// before pg_ctl binds is acceptable for a single-process
// verifier.
func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// probeSelect1 runs `SELECT 1` via psql; returns the first
// non-nil error encountered.  Used by both recovery and
// non-recovery paths because postmaster acceptance of any
// query is the bare-minimum signal.
//
// `-w` tells psql never to prompt for a password (issue #85).
// Otherwise an interactive run blocks the restore at a
// "Password for user postgres:" prompt that automation can
// never fill; even a non-interactive run can stall on stdin.
// Failed authentication errors out cleanly instead.
func probeSelect1(ctx context.Context, psql, dsn string) error {
	cmd := exec.CommandContext(ctx, psql, "-At", "-w", "-c", "select 1", dsn)
	cmd.WaitDelay = probeWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w (output: %s)", err, truncate(out, 256))
	}
	if strings.TrimSpace(string(out)) != "1" {
		return fmt.Errorf("unexpected output %q", string(out))
	}
	return nil
}

// pickProbeDSN tries the Unix-socket DSN first and falls back to
// the TCP DSN if the socket attempt fails.  Returns the DSN that
// worked plus a short label ("socket" or "tcp") for the Result.
// When BOTH attempts fail it joins both errors so the operator can
// see what each path failed on; that diagnostic value matters more
// than a single succinct error.
//
// The preference for socket reflects two facts about a freshly-
// started postverify postmaster: pg_hba.conf's default `local`
// line is peer auth (no password), and the socket directory is
// already pinned to opts.DataDir by `unix_socket_directories`, so
// there is no ambiguity about which postmaster we reach.  The
// reporter's issue (#85) was that the TCP-only path triggered a
// scram-sha-256 password prompt against the source's pg_hba.conf
// even though the local socket would have authenticated cleanly.
func pickProbeDSN(ctx context.Context, psql, socketDSN, tcpDSN string) (string, string, error) {
	sockErr := probeSelect1(ctx, psql, socketDSN)
	if sockErr == nil {
		return socketDSN, "socket", nil
	}
	tcpErr := probeSelect1(ctx, psql, tcpDSN)
	if tcpErr == nil {
		return tcpDSN, "tcp", nil
	}
	return "", "", fmt.Errorf("both socket and tcp probes failed: socket=%v; tcp=%v",
		sockErr, tcpErr)
}

// probeCatalogs runs catalog-level sanity queries.  These
// would fail if pg_class / pg_database is corrupt, which
// would NOT be caught by L1/L2 alone.
//
// `-w` mirrors probeSelect1's no-password posture (issue #85).
func probeCatalogs(ctx context.Context, psql, dsn string) error {
	cmd := exec.CommandContext(ctx, psql, "-At", "-w", "-c",
		"SELECT count(*) FROM pg_database WHERE datallowconn",
		dsn)
	cmd.WaitDelay = probeWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pg_database query: %w (output: %s)", err, truncate(out, 256))
	}
	// At least 1 (postgres / template* DBs).  Zero would be
	// catastrophic and means catalogs are toast.
	if v := strings.TrimSpace(string(out)); v == "0" || v == "" {
		return fmt.Errorf("pg_database returned %q (catalogs corrupt?)", v)
	}
	return nil
}

// readTail returns up to n bytes from the end of path, or
// empty string on any error.  Best-effort diagnostic.
func readTail(path string, n int) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(body) <= n {
		return string(body)
	}
	return string(body[len(body)-n:])
}

// truncate caps a byte slice at n bytes, appending "…" when
// trimmed.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// Note: the drop-to-postgres path (pickPostverifyUser,
// applyDropTo, chownTreeAsRoot, touchAsUser, chmodAReadX,
// chmodAncestorsTraversable) was removed when the CLI started
// refusing euid 0 outright — see internal/cli/refuse_root.go.
// pg_ctl just runs as whatever non-root user the agent runs as,
// PG accepts that uid (its only check is geteuid() != 0), and
// the repo / data-dir permission dance the old path patched
// around no longer happens because the agent owns those files
// directly.

// stageForRecovery primes the restored PGDATA so postverify's
// pg_ctl start can reach a consistent state.  Three artefacts:
//
//	standby.signal               empty file → PG enters
//	                             STANDBY recovery on startup
//	                             (vs `recovery.signal` →
//	                             ARCHIVE recovery, which
//	                             requires a `restore_command`
//	                             and aborts startup with
//	                             FATAL: must specify
//	                             "restore_command" when
//	                             standby mode is not enabled
//	                             if we don't ship one).
//
//	postgresql.auto.conf:        recovery_target = 'immediate'
//	                             + recovery_target_action =
//	                             'promote' → stop replaying
//	                             WAL the moment a consistent
//	                             snapshot is reached, then
//	                             promote so probeSelect1 +
//	                             friends can connect.
//
//	restore_command (CONDITIONAL on repoURL + deployment + agent
//	                 being known): `<agent> wal fetch <dep> %f %p
//	                                 --repo <url>`.  PG invokes this
//	                                 for every WAL segment redo
//	                                 asks for between checkpoint
//	                                 LSN and consistency point.
//	                                 Without restore_command, PG
//	                                 sits in standby waiting for
//	                                 the basebackup STOP-LSN
//	                                 segment to materialise in
//	                                 pg_wal/ — but
//	                                 `pg_hardstorage backup`
//	                                 does NOT bundle trailing
//	                                 WAL into the restored
//	                                 datadir, so the wait is
//	                                 forever and postverify
//	                                 times out.  This was the
//	                                 headline finding of
//	                                 soak testing (10 of 16 soak
//	                                 cells failed for exactly
//	                                 this reason).
//
// We APPEND to postgresql.auto.conf rather than overwrite so a
// caller-supplied auto.conf (e.g. test fixtures forcing a
// specific listen_addresses or shared_preload_libraries) is
// preserved; PG resolves duplicate keys by last-writer-wins,
// which gives our recovery_target settings precedence.
//
// Ownership: the agent runs as a non-root system user (the CLI
// gate at internal/cli/refuse_root.go refuses euid 0), and the
// restore step that produced this dataDir already wrote its
// files under that same uid.  No chown step needed here.
func stageForRecovery(dataDir, repoURL, deployment, agentBinary string, pitrTargetArmed bool) error {
	willHaveRestoreCommand := repoURL != "" && deployment != "" && pickAgentBinary(agentBinary) != ""

	// PITR restore (`restore --to / --to-lsn / --to-name`) already
	// wrote recovery.signal AND an explicit recovery_target_* block via
	// restore.WriteRecoveryFiles.  Appending our own
	// `recovery_target = 'immediate'` on top makes PG refuse to start —
	//
	//   FATAL: multiple recovery targets specified
	//   DETAIL: At most one of "recovery_target", "recovery_target_lsn",
	//   "recovery_target_name", "recovery_target_time",
	//   "recovery_target_xid" may be set.
	//
	// (issue #56).  Leave the operator's target, action and signal file
	// untouched; only ensure a restore_command is present so redo can
	// fetch the WAL needed to reach the target — WriteRecoveryFiles does
	// not write one, and without it archive recovery can't advance past
	// the basebackup's embedded WAL slice to the requested target.
	if pitrTargetArmed {
		if !willHaveRestoreCommand {
			return nil
		}
		return appendRestoreCommand(dataDir, repoURL, deployment, agentBinary)
	}

	// Signal-file choice drives PG's startup mode and determines
	// whether `recovery_target = 'immediate'` is honoured.
	//
	//   recovery.signal → archive-recovery mode.  Requires a
	//                    `restore_command`.  recovery_target =
	//                    'immediate' STOPS replay at the first
	//                    consistent state, then runs
	//                    recovery_target_action (we set =
	//                    'promote').  This is what we want for
	//                    postverify: replay just enough to prove
	//                    the basebackup is bootable, then exit.
	//
	//   standby.signal → standby mode.  PG keeps reading WAL
	//                    indefinitely, waiting for new segments
	//                    from a primary / archive.  recovery_target
	//                    settings are NOT honoured here — recovery
	//                    doesn't "end", so 'immediate' has nothing
	//                    to trigger off.  pg_ctl -w start times
	//                    out forever.
	//
	// Soak testing originally flipped this to standby.signal because
	// at the time there was no restore_command and archive recovery
	// refused to start with `FATAL: must specify "restore_command"
	// when standby mode is not enabled`.  walfetchcmd.Build now
	// gives us a restore_command, so we can switch back to
	// recovery.signal where recovery_target=immediate actually
	// does its job — confirmed in soak testing where 28 of 30 soak
	// cells timed out exactly this way (pg_ctl waiting on
	// segments past the basebackup's stop_lsn while standby
	// mode happily replayed forever).
	//
	// The fallback: callers that don't supply RepoURL +
	// Deployment have no restore_command to wire and would hit
	// the original PG complaint.  For those, we drop back to
	// standby.signal so PG at least starts (it'll then sit
	// waiting for WAL it'll never get, which is fine — the
	// caller signed up for that by not telling us where the
	// repo is).
	signalName := "recovery.signal"
	if !willHaveRestoreCommand {
		signalName = "standby.signal"
	}
	signalPath := filepath.Join(dataDir, signalName)
	if err := touchAsRoot(signalPath); err != nil {
		return fmt.Errorf("touch %s: %w", signalName, err)
	}

	autoConf := filepath.Join(dataDir, "postgresql.auto.conf")
	f, err := os.OpenFile(autoConf, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open postgresql.auto.conf: %w", err)
	}
	body := "\n# pg_hardstorage postverify (auto-appended):\n" +
		"recovery_target = 'immediate'\n" +
		"recovery_target_action = 'promote'\n"
	// When the caller wired RepoURL + Deployment AND we resolved
	// an agent binary, install a restore_command so PG can pull
	// the trailing WAL segment(s) out of the repo as redo asks
	// for them.  The willHaveRestoreCommand check above keeps the
	// signal-file selection aligned with whether this branch
	// fires; the two MUST agree (recovery.signal without
	// restore_command would have PG refuse to start).
	if willHaveRestoreCommand {
		// PG passes %f (segment name) and %p (target path) into
		// restore_command.  Goes through walfetchcmd.Build so the
		// exit-6 → exit-1 wrapper is applied — see that package's
		// docstring for the full rationale (restore sandbox recovery
		// loop).  walfetchcmd.Build emits POSIX-quoted args
		// (single quotes around bin/deployment/repoURL) so we
		// double the single quotes here for PG's SQL-string-literal
		// escape — wrapping in raw `'…'` would close the PG string
		// at the first inner apostrophe and break the GUC parser.
		rawCmd := walfetchcmd.Build(pickAgentBinary(agentBinary), deployment, repoURL)
		body += fmt.Sprintf("restore_command = '%s'\n",
			strings.ReplaceAll(rawCmd, "'", "''"))
	}
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		return fmt.Errorf("append recovery_target settings: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close postgresql.auto.conf: %w", err)
	}

	return nil
}

// appendRestoreCommand appends ONLY a `restore_command` line to
// postgresql.auto.conf.  Used by the PITR-armed path of
// stageForRecovery, where the recovery_target_* block + recovery.signal
// already exist (written by restore.WriteRecoveryFiles) and the only
// thing missing for redo to reach the operator's target is the WAL-fetch
// command.  Deliberately does NOT touch recovery_target / the signal
// file — see the issue #56 note in stageForRecovery.
func appendRestoreCommand(dataDir, repoURL, deployment, agentBinary string) error {
	autoConf := filepath.Join(dataDir, "postgresql.auto.conf")
	f, err := os.OpenFile(autoConf, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open postgresql.auto.conf: %w", err)
	}
	// walfetchcmd.Build emits POSIX-quoted args; double the single
	// quotes for PG's SQL-string-literal escape (same as the non-PITR
	// branch above).
	rawCmd := walfetchcmd.Build(pickAgentBinary(agentBinary), deployment, repoURL)
	body := "\n# pg_hardstorage postverify (PITR restore_command):\n" +
		fmt.Sprintf("restore_command = '%s'\n", strings.ReplaceAll(rawCmd, "'", "''"))
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		return fmt.Errorf("append restore_command: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close postgresql.auto.conf: %w", err)
	}
	return nil
}

// pickAgentBinary resolves the operator-supplied agentBinary or
// falls back to os.Executable() (the running agent binary itself,
// which is the right choice when postverify runs in-process under
// the agent).  Returns "" when neither path is usable — callers
// treat that as "no restore_command available" and drop back to
// standby.signal mode.
func pickAgentBinary(operatorSupplied string) string {
	if bin := strings.TrimSpace(operatorSupplied); bin != "" {
		return bin
	}
	if self, err := os.Executable(); err == nil {
		return self
	}
	return ""
}

// touchAsRoot creates an empty file (no-op if it already
// exists).  Used by stageForRecovery for recovery.signal.
func touchAsRoot(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}
