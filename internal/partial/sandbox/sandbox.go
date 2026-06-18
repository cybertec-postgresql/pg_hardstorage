// Package sandbox runs a local PostgreSQL instance against a
// restored data directory long enough to extract per-table SQL
// via pg_dump. This is the SQL-emitting half of partial restore
// the plan documented as deferred:
//
//	"ships ... partial restore — the actual table extraction
//	 into a running DB ... ships alongside the sandbox-PG
//	 verifier."
//
// The flow:
//
//  1. Caller has a fully-restored data dir at DataDir.
//  2. Sandbox writes a tiny postgresql.auto.conf disabling TCP
//     (`port = 0`, `listen_addresses = ”`) and pointing
//     unix_socket_directories at a tempdir we own. Operators
//     running pg_hardstorage on hosts where ports might be in
//     use don't get a port-collision panic.
//  3. We spawn `pg_ctl start -D <DataDir> -l <log>` and wait up
//     to StartupTimeout for the socket file to appear (which
//     means PG is accepting queries on the unix socket).
//  4. Caller runs Dump(...) for each table-set. Internally that's
//     `pg_dump --host=<sockdir> --table=<schema>.<name> ...`.
//  5. Caller calls Stop() (or relies on Close-via-defer); we run
//     `pg_ctl stop -D <DataDir> -m fast` and clean up the
//     socket dir.
//
// What's deliberately NOT in scope:
//
//   - Restoring the backup into DataDir. That's the restore
//     package's job; the operator runs `pg_hardstorage restore`
//     first, then this. Composing them into a single
//     `partial dump` CLI is the layer above this package.
//   - testcontainers-style isolation. Spawning local pg_ctl is
//     simpler and matches the plan's "operators have PG binaries
//     on PATH" assumption. A future testcontainers path can wrap
//     this primitive without changing its surface.
//   - PG-version negotiation: the data dir's PG_VERSION file vs
//     pg_ctl --version is now pre-flighted at Start time (closes
//     the explicit deferral from the partial-dump commit). A
//     mismatch surfaces a structured sandbox.pg_version_mismatch
//     error before pg_ctl is invoked, so an operator missing the
//     matching PG binary set sees a clean diagnostic rather than
//     PG's own opaque "incompatible cluster" or segfault.
//
// Operating-system contract: the host needs `pg_ctl` and `pg_dump`
// on PATH (or the operator passes explicit paths). The package
// invokes them as subprocesses; no libpq linkage required.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
)

// DefaultStartupTimeout is how long we wait for pg_ctl start +
// the socket-file-appears handshake before giving up.
const DefaultStartupTimeout = 60 * time.Second

// DefaultShutdownTimeout is how long we wait for pg_ctl stop.
const DefaultShutdownTimeout = 30 * time.Second

// Options configures Start. Required: DataDir.
type Options struct {
	// DataDir is the path to a fully-restored PG data directory.
	// pg_ctl will start the cluster against this. Must already
	// have correct mode (0700) and ownership.
	DataDir string

	// PGCtlPath overrides the discovered pg_ctl binary. Empty
	// means "look it up on PATH at Start time".
	PGCtlPath string

	// PGDumpPath overrides the discovered pg_dump binary. Empty
	// means "look it up on PATH at Dump time".
	PGDumpPath string

	// StartupTimeout bounds how long we wait for the socket file
	// to appear after pg_ctl start. Zero uses DefaultStartupTimeout.
	StartupTimeout time.Duration

	// ShutdownTimeout bounds how long we wait for pg_ctl stop.
	// Zero uses DefaultShutdownTimeout.
	ShutdownTimeout time.Duration

	// Database is the database to connect to for pg_dump invocations.
	// Defaults to "postgres" (always present on a freshly-restored
	// cluster).
	Database string

	// Stderr, when non-nil, receives the subprocess stderr streams
	// for debugging. nil discards.
	Stderr io.Writer

	// SkipVersionCheck disables the data-dir vs pg_ctl version
	// pre-flight. Default-off; flipping it on is for operators
	// running heterogeneous fleets where they've validated
	// compatibility some other way (e.g. a custom pg_ctl wrapper
	// that handles version routing). The audit log records that
	// the gate was bypassed.
	SkipVersionCheck bool
}

// Sandbox is one running PG instance + helper state. Construct
// via Start; release via Stop (or defer).
type Sandbox struct {
	opts          Options
	pgCtl         string
	pgDump        string
	socketDir     string
	logFile       string
	startupConfig string

	closed bool
}

// Start brings up a sandbox PG against opts.DataDir. Returns when
// the socket file is responsive, or an error.
//
// On error, any partial state (the socket-dir tempdir, the auto.conf
// override) is cleaned up before returning.
func Start(ctx context.Context, opts Options) (*Sandbox, error) {
	if opts.DataDir == "" {
		return nil, errors.New("sandbox: DataDir is required")
	}
	if _, err := os.Stat(opts.DataDir); err != nil {
		return nil, fmt.Errorf("sandbox: stat DataDir: %w", err)
	}
	pgCtl := opts.PGCtlPath
	if pgCtl == "" {
		p, err := exec.LookPath("pg_ctl")
		if err != nil {
			return nil, fmt.Errorf("sandbox: pg_ctl not on PATH: %w (set Options.PGCtlPath)", err)
		}
		pgCtl = p
	}
	pgDump := opts.PGDumpPath
	if pgDump == "" {
		p, err := exec.LookPath("pg_dump")
		if err != nil {
			return nil, fmt.Errorf("sandbox: pg_dump not on PATH: %w (set Options.PGDumpPath)", err)
		}
		pgDump = p
	}
	startupTimeout := opts.StartupTimeout
	if startupTimeout == 0 {
		startupTimeout = DefaultStartupTimeout
	}
	if opts.Database == "" {
		opts.Database = "postgres"
	}

	// PG-version pre-flight: refuse to spawn pg_ctl against a data
	// dir whose PG_VERSION doesn't match the binary's major
	// version. The kernel cost of a forked pg_ctl on the wrong
	// data dir is real (segfault, partial WAL writes, the works);
	// failing fast saves the operator from a confusing crash
	// report. SkipVersionCheck escapes the gate for operators
	// running heterogeneous fleets where they've already
	// validated compatibility some other way.
	if !opts.SkipVersionCheck {
		dataDirMajor, derr := readDataDirPGVersion(opts.DataDir)
		if derr != nil {
			return nil, fmt.Errorf("sandbox: read data-dir PG_VERSION: %w", derr)
		}
		binaryMajor, berr := pgCtlMajorVersion(ctx, pgCtl)
		if berr != nil {
			return nil, fmt.Errorf("sandbox: probe pg_ctl --version: %w", berr)
		}
		if dataDirMajor != binaryMajor {
			return nil, &VersionMismatchError{
				DataDirMajor: dataDirMajor,
				BinaryMajor:  binaryMajor,
				PGCtlPath:    pgCtl,
				DataDir:      opts.DataDir,
				BinaryName:   "pg_ctl",
			}
		}

		// pg_dump version too. An operator with split installs
		// (pg_ctl from one major, pg_dump from another — possible
		// with manual PATH manipulation or vendored binaries)
		// would hit the mismatch later when Dump() runs, with
		// pg_dump's own opaque "version mismatch" error. Catch it
		// here at startup so the diagnostic is structured.
		dumpMajor, derr := pgBinaryMajorVersion(ctx, pgDump)
		if derr != nil {
			return nil, fmt.Errorf("sandbox: probe pg_dump --version: %w", derr)
		}
		if dataDirMajor != dumpMajor {
			return nil, &VersionMismatchError{
				DataDirMajor: dataDirMajor,
				BinaryMajor:  dumpMajor,
				PGCtlPath:    pgDump,
				DataDir:      opts.DataDir,
				BinaryName:   "pg_dump",
			}
		}
	}

	// Allocate a host-tempdir for our socket so we don't collide
	// with whatever's in /tmp. Names of unix socket files are
	// significant — pg_dump --host accepts a directory path and
	// constructs `<dir>/.s.PGSQL.<port>` itself.
	sockDir, err := os.MkdirTemp("", "pg_hardstorage-sandbox-sock-*")
	if err != nil {
		return nil, fmt.Errorf("sandbox: mkdir socket dir: %w", err)
	}
	// PG insists on socket dirs being mode 0700.
	if err := os.Chmod(sockDir, 0o700); err != nil {
		os.RemoveAll(sockDir)
		return nil, fmt.Errorf("sandbox: chmod socket dir: %w", err)
	}

	// Write postgresql.auto.conf for the sandbox.  It must:
	//   - disable TCP entirely (listen_addresses='') and point
	//     unix_socket_directories at our sockDir;
	//   - PRESERVE whatever the restore step already wrote there.
	//
	// Issue #96: a partial-dump restore arms auto-recovery in this same
	// auto.conf (a restore_command plus standby.signal) so PG can fetch
	// WAL and reach consistency before pg_dump reads anything.  The
	// sandbox used to *replace* the file with socket-only settings,
	// stashing the restore's version in a
	// `.pg_hardstorage_sandbox_backup` sidecar — which left PG with
	// "specified neither primary_conninfo nor restore_command" and a
	// recovery that never finishes.  Instead we keep the existing
	// content and APPEND our overrides; PG applies auto.conf last-wins
	// line-by-line, so our socket settings take effect without deleting
	// the restore_command PG needs.
	autoConfPath := filepath.Join(opts.DataDir, "postgresql.auto.conf")
	autoConfBackup := autoConfPath + ".pg_hardstorage_sandbox_backup"
	startupConfig := autoConfPath
	var existing []byte
	if body, err := os.ReadFile(autoConfPath); err == nil {
		existing = body
		// Preserve the operator's existing auto.conf so we can
		// restore it byte-for-byte on Stop.
		// fsutil.WriteFileAtomic: a torn backup file would silently
		// destroy the operator's original auto.conf on Stop.
		if werr := fsutil.WriteFileAtomic(autoConfBackup, existing, 0o600); werr != nil {
			os.RemoveAll(sockDir)
			return nil, fmt.Errorf("sandbox: backup auto.conf: %w", werr)
		}
	}
	conf := buildSandboxAutoConf(existing, sockDir)
	// fsutil.WriteFileAtomic: PG reads auto.conf at startup; an
	// in-place os.WriteFile could leave a partially-written file
	// visible if a crash interrupted it between the two failed
	// resumes (sandbox cleanup also tries to read this back).
	if err := fsutil.WriteFileAtomic(autoConfPath, conf, 0o600); err != nil {
		os.RemoveAll(sockDir)
		os.Remove(autoConfBackup)
		return nil, fmt.Errorf("sandbox: write auto.conf: %w", err)
	}

	logFile := filepath.Join(sockDir, "pg.log")

	sb := &Sandbox{
		opts:          opts,
		pgCtl:         pgCtl,
		pgDump:        pgDump,
		socketDir:     sockDir,
		logFile:       logFile,
		startupConfig: startupConfig,
	}

	// Start PG.
	cmd := exec.CommandContext(ctx, pgCtl, "start",
		"-D", opts.DataDir,
		"-l", logFile,
		"-w",
		"-t", fmt.Sprintf("%d", int(startupTimeout/time.Second)),
	)
	if opts.Stderr != nil {
		cmd.Stderr = opts.Stderr
	}
	if err := cmd.Run(); err != nil {
		sb.cleanup()
		// Best-effort: capture the log so the operator sees what
		// went wrong.
		var logTail string
		if body, rerr := os.ReadFile(logFile); rerr == nil {
			logTail = tailString(string(body), 4096)
		}
		return nil, fmt.Errorf("sandbox: pg_ctl start: %w (log tail: %s)", err, logTail)
	}

	// pg_ctl -w should have waited for readiness; double-check the
	// socket file exists.
	socketPath := filepath.Join(sockDir, ".s.PGSQL.5432")
	if _, err := os.Stat(socketPath); err != nil {
		sb.cleanup()
		return nil, fmt.Errorf("sandbox: PG started but socket %s not found: %w", socketPath, err)
	}

	return sb, nil
}

// buildSandboxAutoConf returns the postgresql.auto.conf body the sandbox
// runs PG with: the restore step's existing content preserved verbatim
// (so restore_command + any recovery GUCs survive — issue #96) followed
// by the sandbox's socket/TCP overrides.  PG applies auto.conf
// last-wins, so the appended overrides take effect without dropping the
// recovery settings PG needs to replay WAL.
func buildSandboxAutoConf(existing []byte, sockDir string) []byte {
	overrides := fmt.Sprintf(
		"# pg_hardstorage sandbox overrides — restored on Stop\n"+
			"port = 5432\n"+ // socket-file name suffix
			"listen_addresses = ''\n"+
			"unix_socket_directories = '%s'\n",
		sockDir,
	)
	if len(existing) == 0 {
		return []byte(overrides)
	}
	var b bytes.Buffer
	b.Write(existing)
	if !bytes.HasSuffix(existing, []byte("\n")) {
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(overrides)
	return b.Bytes()
}

// SocketDir returns the directory containing the unix socket file.
// `pg_dump --host=<this-value>` will connect via the socket.
func (s *Sandbox) SocketDir() string { return s.socketDir }

// LogFile returns the path PG writes its server log to. Useful for
// post-mortem inspection when something inside the sandbox goes
// wrong.
func (s *Sandbox) LogFile() string { return s.logFile }

// Dump runs pg_dump for the named tables and writes the SQL to w.
// Each table is qualified `schema.name`; pg_dump's `--table=` flag
// accepts the same form.
//
// Format options:
//   - When DataOnly is true, pg_dump runs with --data-only (no DDL).
//   - Otherwise the default plain SQL output (DDL + data) flows.
//
// The caller is responsible for the bytes' destination — we don't
// open files, we just stream pg_dump's stdout into w.
func (s *Sandbox) Dump(ctx context.Context, w io.Writer, tables []string, dataOnly bool) error {
	if s.closed {
		return errors.New("sandbox: Dump after Stop")
	}
	if len(tables) == 0 {
		return errors.New("sandbox: Dump requires at least one table")
	}
	args := buildPGDumpArgs(s.socketDir, currentUser(), s.opts.Database, tables, dataOnly)

	// Always capture pg_dump's stderr so a "no matching tables were
	// found" diagnostic (issue #97: a table that lives in a database
	// other than the one we connected to) reaches the operator instead
	// of being discarded.  When Options.Stderr is set we tee to both.
	var errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, s.pgDump, args...)
	cmd.Stdout = w
	if s.opts.Stderr != nil {
		cmd.Stderr = io.MultiWriter(s.opts.Stderr, &errBuf)
	} else {
		cmd.Stderr = &errBuf
	}
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errBuf.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, tailString(msg, 2048))
		}
		return err
	}
	return nil
}

// buildPGDumpArgs assembles the pg_dump argv for a sandbox dump.  The
// database name is the final positional argument; pg_dump resolves
// --table only within that database, which is exactly why the caller
// must be able to choose it (issue #97).
func buildPGDumpArgs(socketDir, user, database string, tables []string, dataOnly bool) []string {
	args := []string{
		"--host=" + socketDir,
		"--port=5432",
		"--username=" + user,
		"--no-password", // unix-socket peer auth → no password prompt
	}
	if dataOnly {
		args = append(args, "--data-only")
	}
	for _, t := range tables {
		args = append(args, "--table="+t)
	}
	if database != "" {
		args = append(args, database)
	}
	return args
}

// Stop shuts down the PG instance and removes the socket dir +
// the postgresql.auto.conf override. Idempotent: safe to call
// twice. Always restores the operator's prior auto.conf if there
// was one.
func (s *Sandbox) Stop(ctx context.Context) error {
	if s.closed {
		return nil
	}
	s.closed = true

	timeout := s.opts.ShutdownTimeout
	if timeout == 0 {
		timeout = DefaultShutdownTimeout
	}
	stopCmd := exec.CommandContext(ctx, s.pgCtl, "stop",
		"-D", s.opts.DataDir,
		"-m", "fast",
		"-w",
		"-t", fmt.Sprintf("%d", int(timeout/time.Second)),
	)
	if s.opts.Stderr != nil {
		stopCmd.Stderr = s.opts.Stderr
	}
	stopErr := stopCmd.Run()

	s.cleanup()
	return stopErr
}

// cleanup is called from both the Stop happy path and the Start
// error path. Removes the socket dir + restores the
// postgresql.auto.conf if we backed one up.
func (s *Sandbox) cleanup() {
	autoConfPath := filepath.Join(s.opts.DataDir, "postgresql.auto.conf")
	autoConfBackup := autoConfPath + ".pg_hardstorage_sandbox_backup"
	if body, err := os.ReadFile(autoConfBackup); err == nil {
		// Best-effort restore; cleanup() is the rollback path so we
		// don't surface the error, but the durable write keeps the
		// operator from losing their config to a crash mid-restore.
		_ = fsutil.WriteFileAtomic(autoConfPath, body, 0o600)
		_ = os.Remove(autoConfBackup)
	} else {
		// No backup → we wrote the file from scratch. Remove it.
		_ = os.Remove(autoConfPath)
	}
	if s.socketDir != "" {
		_ = os.RemoveAll(s.socketDir)
	}
}

// tailString returns the last n bytes of s, with a leading
// "...<truncated>..." marker when truncation happened.
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...<truncated>...\n" + s[len(s)-n:]
}

// currentUser returns $USER or "postgres" as a fallback. PG
// peer-auth checks the connecting OS user against the requested
// PG role. We pass --username explicitly so pg_dump doesn't
// reflect a stale one from libpq env vars the operator might
// have set.
func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("LOGNAME"); u != "" {
		return u
	}
	return "postgres"
}

// DiscoverPGTools probes PATH for pg_ctl + pg_dump and returns
// the resolved paths. Useful in CLI pre-flight checks before the
// operator commits to a long restore.
func DiscoverPGTools() (pgCtl, pgDump string, err error) {
	pgCtl, err = exec.LookPath("pg_ctl")
	if err != nil {
		return "", "", fmt.Errorf("pg_ctl: %w", err)
	}
	pgDump, err = exec.LookPath("pg_dump")
	if err != nil {
		return "", "", fmt.Errorf("pg_dump: %w", err)
	}
	return pgCtl, pgDump, nil
}

// Sanity import keeping bytes referenced for future tail-trim
// helpers.
var _ = bytes.NewReader

// Sanity import keeping strings referenced.
var _ = strings.HasPrefix

// readDataDirPGVersion reads the data dir's PG_VERSION file and
// returns the parsed major version. Every PG-initialised data dir
// has this file; its absence is a clear "this isn't a PG data
// dir" signal and we surface that explicitly rather than letting
// pg_ctl segfault on it.
//
// File contents: a single ASCII line with the major version,
// e.g., "17\n". PG 9.x and earlier used "9.6"-style minor in this
// file; we accept the leading integer-prefix as the major.
func readDataDirPGVersion(dataDir string) (int, error) {
	path := filepath.Join(dataDir, "PG_VERSION")
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("PG_VERSION absent at %q: this does not look like a PG data directory (was the restore complete?)", dataDir)
		}
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	major, err := parsePGVersionContent(string(body))
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	return major, nil
}

// parsePGVersionContent extracts the major version from PG_VERSION
// content. Tolerant of trailing whitespace; tolerant of "9.6"
// minor-included shapes by taking the leading integer.
func parsePGVersionContent(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("PG_VERSION is empty")
	}
	// Take the leading run of digits.
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, fmt.Errorf("no leading digits in %q", s)
	}
	var n int
	for i := 0; i < end; i++ {
		n = n*10 + int(s[i]-'0')
	}
	if n == 0 {
		return 0, fmt.Errorf("major version 0 from %q", s)
	}
	return n, nil
}

// pgCtlMajorVersion runs `pg_ctl --version` and parses the major
// version from its output. Backwards-compat alias preserving the
// pg_ctl-only call site; pgBinaryMajorVersion is the generic
// helper that also covers pg_dump.
func pgCtlMajorVersion(ctx context.Context, pgCtlPath string) (int, error) {
	return pgBinaryMajorVersion(ctx, pgCtlPath)
}

// pgBinaryMajorVersion runs `<binary> --version` and parses the
// major version. Generalised across pg_ctl and pg_dump (and any
// future PG client binary that follows the same `<name>
// (PostgreSQL) <version>` banner format).
//
// Output shapes (PG 15+):
//
//	pg_ctl (PostgreSQL) 17.2
//	pg_ctl (PostgreSQL) 16.4
//	pg_dump (PostgreSQL) 17.2
//
// Vendor banners are tolerated by parsePGCtlVersionOutput's
// "last token containing a dot" heuristic.
func pgBinaryMajorVersion(ctx context.Context, binaryPath string) (int, error) {
	cmd := exec.CommandContext(ctx, binaryPath, "--version")
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("run %s --version: %w", binaryPath, err)
	}
	major, err := parsePGCtlVersionOutput(string(out))
	if err != nil {
		return 0, fmt.Errorf("parse %s --version output %q: %w", binaryPath, string(out), err)
	}
	return major, nil
}

// parsePGCtlVersionOutput extracts the major version from a
// pg_ctl --version line. We look for the LAST whitespace-
// separated token containing a `.` (the version literal); take
// the prefix before the first `.` as the major.
//
// Defensive: PG distros have been known to inject vendor strings
// (e.g., "pg_ctl (EnterpriseDB PostgreSQL) 17.2 (custom)"); the
// last-dot-bearing-token-strategy tolerates them as long as the
// real version string is present.
func parsePGCtlVersionOutput(s string) (int, error) {
	s = strings.TrimSpace(s)
	// First line only — defensive against multi-line vendor banners.
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	tokens := strings.Fields(s)
	var version string
	for _, tok := range tokens {
		if strings.Contains(tok, ".") {
			version = tok
		}
	}
	if version == "" {
		return 0, errors.New("no version-shaped token in output")
	}
	// version is like "17.2" or "17.2beta1"; major is the prefix
	// up to the first non-digit.
	end := 0
	for end < len(version) && version[end] >= '0' && version[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, fmt.Errorf("token %q does not start with digits", version)
	}
	var n int
	for i := 0; i < end; i++ {
		n = n*10 + int(version[i]-'0')
	}
	if n == 0 {
		return 0, fmt.Errorf("major version 0 from %q", version)
	}
	return n, nil
}

// VersionMismatchError reports a data-dir-vs-binary major-version
// disagreement. Carries both versions, the binary path, and the
// binary's well-known short name so an operator-facing CLI
// renderer can produce an actionable error pointing at exactly
// which binary needs replacing.
//
// Today the gate fires for pg_ctl and pg_dump (both checked at
// Start time so a wrong-version pg_dump is caught up-front
// rather than during Dump()). PGCtlPath is named for legacy
// reasons; it carries whatever path the mismatch was detected
// at — pg_ctl or pg_dump depending on BinaryName.
type VersionMismatchError struct {
	DataDirMajor int
	BinaryMajor  int
	PGCtlPath    string // path to the offending binary (pg_ctl or pg_dump)
	DataDir      string
	BinaryName   string // "pg_ctl" or "pg_dump"
}

// Error implements error.
func (e *VersionMismatchError) Error() string {
	name := e.BinaryName
	if name == "" {
		name = "pg_ctl" // legacy default — earlier callers didn't set BinaryName
	}
	return fmt.Sprintf("sandbox: data dir %q is PG %d but %s at %q is PG %d (install the matching PostgreSQL major, or set Options.%sPath, or pass --skip-version-check to bypass)",
		e.DataDir, e.DataDirMajor, name, e.PGCtlPath, e.BinaryMajor, optionsPathField(name))
}

// optionsPathField maps a binary short name to the corresponding
// Options field name so the error suggestion points at the right
// override flag.
func optionsPathField(name string) string {
	switch name {
	case "pg_dump":
		return "PGDump"
	default:
		return "PGCtl"
	}
}

// Is implements errors.Is so callers can match on the sentinel.
func (e *VersionMismatchError) Is(target error) bool {
	return target == ErrPGVersionMismatch
}

// ErrPGVersionMismatch is the sentinel for errors.Is.
var ErrPGVersionMismatch = errors.New("sandbox: PG version mismatch between data dir and binary")
