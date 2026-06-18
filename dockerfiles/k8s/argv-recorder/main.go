// argv-recorder — drop-in fixture-capture shim for operator
// images.  Replaces the original backup tool binary
// (barman-cloud-backup / pgbackrest / wal-g) and, when invoked,
// appends one JSON line to a fixture file describing the exact
// argv + environment the operator passed, then exec's the real
// tool that's been moved aside to <basename>.real.
//
// Why?  Each Postgres operator hands its backup tool a slightly
// different argv shape (env vars, flags, positional args).
// Reading three operators' source code to enumerate every
// invocation is tedious and goes stale.  Running each operator
// once with this shim in place yields a fixture file that
// describes EXACTLY what the operator does — feeds straight
// into the compat-shim regression tests.
//
// Wire-up (in the per-operator shim Dockerfile):
//
//	mv /usr/bin/barman-cloud-backup /usr/bin/barman-cloud-backup.real
//	cp argv-recorder /usr/bin/barman-cloud-backup
//
// Then run the operator + a backup; check
// /var/log/pg_hardstorage/argv-fixture.ndjson on the pod.
//
// The recorder is intentionally tiny and self-contained — it
// runs in the operator's pod, so it must have zero runtime
// dependencies and not panic on weird input.  Failures are
// best-effort: if the fixture file can't be written (read-only
// fs, missing directory, etc.) the recorder still exec's the
// real tool — operator backups never break because the
// fixture is unwritable.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// fixturePath is where invocations land.  Override via
// PGHS_ARGV_FIXTURE for tests.  Default lives under /tmp
// because /var/log is mounted read-only in most operator
// images (CNPG explicitly does this as defense-in-depth) and
// /tmp is reliably writable in every container.  Operators
// who want a persistent path mount one and pass
// PGHS_ARGV_FIXTURE.
const defaultFixturePath = "/tmp/argv-fixture.ndjson"

// fixtureStderr controls whether each entry is also written
// to stderr.  Always on — the operator's pod logs become a
// backup capture channel when the file write fails (read-only
// fs, ENOSPC, missing parent dir).  Cheap insurance: a tiny
// JSON line per archive is dwarfed by the operator's own
// barman-cloud output.
const fixtureStderr = true

// realBinarySuffix is what the install path appends to the
// original tool when moving it aside.  We exec
// `<argv[0]>.real` after recording.
const realBinarySuffix = ".real"

type fixtureEntry struct {
	At       string   `json:"at"`
	Tool     string   `json:"tool"`
	Argv     []string `json:"argv"`
	Env      []string `json:"env"`
	Cwd      string   `json:"cwd,omitempty"`
	Pid      int      `json:"pid"`
	Pod      string   `json:"pod,omitempty"`
	HostName string   `json:"host,omitempty"`
}

func main() {
	tool := filepath.Base(os.Args[0])
	entry := fixtureEntry{
		At:   time.Now().UTC().Format(time.RFC3339Nano),
		Tool: tool,
		Argv: os.Args,
		Env:  filteredEnv(),
		Pid:  os.Getpid(),
	}
	if cwd, err := os.Getwd(); err == nil {
		entry.Cwd = cwd
	}
	// HOSTNAME is the pod name on Kubernetes; no need to
	// shell out to /etc/hostname or the downward API.
	entry.HostName = os.Getenv("HOSTNAME")
	entry.Pod = os.Getenv("POD_NAME")

	writeFixture(entry)
	execReal()
}

// writeFixture appends one JSON line to the fixture file AND,
// when fixtureStderr is on, to stderr.  Best-effort: any
// failure (read-only fs, ENOSPC, missing parent dir) is
// swallowed so the operator's backup workflow keeps running
// — but stderr always works in a container, so the fixture
// is captured one way or the other.
func writeFixture(entry fixtureEntry) {
	body, err := json.Marshal(entry)
	if err != nil {
		return
	}
	body = append(body, '\n')

	// Stderr first — never fails, always shows up in
	// `kubectl logs <pod>`.  The PGHS_ARGV_FIXTURE_PREFIX
	// makes the line easy to grep out of the operator's
	// own barman-cloud chatter.
	if fixtureStderr {
		fmt.Fprintf(os.Stderr, "PGHS_ARGV_FIXTURE %s", string(body))
	}

	// Then the file — may fail; the stderr channel above
	// still has the data for grep-based extraction.
	path := os.Getenv("PGHS_ARGV_FIXTURE")
	if path == "" {
		path = defaultFixturePath
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755) // ignore EROFS / EACCES
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(body)
}

// filteredEnv returns os.Environ() with secret-shaped values
// redacted.  A backup-tool argv-recorder fixture is committed
// into the repo as a regression baseline; we don't want
// AWS_SECRET_ACCESS_KEY or similar leaking.  The match is
// substring-based and case-insensitive; values are replaced
// with the literal "<redacted>" but the key is preserved so
// fixture diffs still show the operator setting the var.
func filteredEnv() []string {
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			out = append(out, kv)
			continue
		}
		key := kv[:eq]
		lk := strings.ToLower(key)
		switch {
		case strings.Contains(lk, "secret"),
			strings.Contains(lk, "password"),
			strings.Contains(lk, "token"),
			strings.Contains(lk, "credential"),
			strings.Contains(lk, "api_key"),
			strings.Contains(lk, "apikey"):
			out = append(out, key+"=<redacted>")
		default:
			out = append(out, kv)
		}
	}
	return out
}

// execReal replaces the recorder process with the original
// tool, located at <selfPath><realBinarySuffix>.  syscall.Exec
// (not exec.Command) so the operator sees its child as the
// tool's PID directly — many operators kill / wait on the PID
// they spawned, and an extra shim process between them and
// the real tool would break that contract.
//
// We resolve via /proc/self/exe rather than os.Args[0]: when
// the operator invokes the tool via PATH lookup ("docker run
// --entrypoint=barman-cloud-backup ..." or `exec.Command(
// "barman-cloud-backup")`), os.Args[0] arrives as the bare
// basename and `<basename>.real` would resolve relative to
// cwd, which is wrong.  /proc/self/exe is always the
// absolute path of the running binary on Linux, exactly what
// we need.  Falls back to os.Args[0] on non-Linux for
// portability of the unit tests.
func execReal() {
	selfPath, err := os.Readlink("/proc/self/exe")
	if err != nil {
		// non-Linux or /proc not mounted; fall back to argv[0]
		// resolved via PATH lookup — exec.LookPath does the
		// PATH walk if argv[0] has no slash.
		selfPath = os.Args[0]
	}
	realPath := selfPath + realBinarySuffix
	if _, err := os.Stat(realPath); err != nil {
		// Real binary missing: emit a clear error to the
		// operator's logs so the misconfiguration is obvious
		// (and test scenarios that forgot to run the
		// install-time `mv` get a loud failure, not a silent
		// successful no-op).
		fmt.Fprintf(os.Stderr,
			"argv-recorder: cannot exec real tool: %s missing.  "+
				"Did the image install step move the original to .real?\n",
			realPath)
		os.Exit(127)
	}
	if err := syscall.Exec(realPath, os.Args, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "argv-recorder: exec %s: %v\n", realPath, err)
		os.Exit(126)
	}
}
