package cli_test

import (
	"bytes"
	stdjson "encoding/json"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// runBackup is a small helper for backup-command tests. It returns
// stdout, stderr, and the exit code so assertions can be made on the
// CLI contract (errors land on stderr; success on stdout).
func runBackup(t *testing.T, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	root := cli.NewRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(append([]string{"backup"}, args...))
	exit = cli.Run(root)
	return out.String(), errb.String(), exit
}

func TestBackup_RequiresDeployment(t *testing.T) {
	_, _, exit := runBackup(t)
	if exit == int(output.ExitOK) {
		t.Error("backup with no args should not succeed")
	}
}

func TestBackup_RequiresPGConnection(t *testing.T) {
	stdout, errb, exit := runBackup(t, "db1", "--repo", "file:///tmp/x", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --pg-connection should map to ExitMisuse(%d); got %d", output.ExitMisuse, exit)
	}
	if stdout != "" {
		t.Errorf("usage error must not land on stdout: %q", stdout)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(errb), &res); err != nil {
		t.Fatalf("invalid JSON on stderr: %v\n%s", err, errb)
	}
	if !res.IsError() {
		t.Fatal("Result should carry an error")
	}
	if res.Error.Code != "usage.missing_flag" {
		t.Errorf("error code = %q, want usage.missing_flag", res.Error.Code)
	}
	if !strings.Contains(res.Error.Message, "pg-connection") {
		t.Errorf("error message should mention pg-connection: %q", res.Error.Message)
	}
}

func TestBackup_RequiresRepo(t *testing.T) {
	_, errb, exit := runBackup(t, "db1", "--pg-connection", "postgres://x@y/z", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --repo should map to ExitMisuse; got %d", exit)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(errb), &res); err != nil {
		t.Fatalf("invalid JSON on stderr: %v\n%s", err, errb)
	}
	if res.Error.Code != "usage.missing_flag" {
		t.Errorf("error code = %q", res.Error.Code)
	}
	if !strings.Contains(res.Error.Message, "repo") {
		t.Errorf("error message should mention repo: %q", res.Error.Message)
	}
}

func TestBackup_BadDSN_StructuredError(t *testing.T) {
	// A malformed DSN is detected by pg.Connect before any I/O — we
	// don't need a real PG to exercise this path. The keystore is
	// auto-generated in the user's keyring (XDG path); the test runs
	// with HOME pointing at a temp dir to keep that contained.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")

	_, errb, exit := runBackup(t,
		"db1",
		"--pg-connection", "this-is-not-a-dsn",
		"--repo", "file://"+t.TempDir(), // doesn't matter — DSN parse fails first
		"-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatal("expected non-zero exit on bad DSN")
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(errb), &res); err != nil {
		t.Fatalf("invalid JSON on stderr: %v\n%s", err, errb)
	}
	if !res.IsError() {
		t.Fatal("expected error result")
	}
	// The error chain hits either probeVersion (regular conn) or repo
	// open (which we'd hit first if we changed the order). Whichever
	// wins, we want a structured error not "internal: <opaque>".
	if res.Error.Code == "" {
		t.Error("error must carry a code")
	}
	t.Logf("got code=%q message=%q", res.Error.Code, res.Error.Message)
}

func TestBackup_BadRepoURL_NotARepo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")

	// Empty directory at a valid file:// URL → repo.Open returns
	// ErrNotARepo → structured "notfound.repo".
	emptyDir := t.TempDir()
	stdout, errb, exit := runBackup(t,
		"db1",
		"--pg-connection", "postgres://x@localhost:5432/postgres",
		"--repo", "file://"+emptyDir,
		"-o", "json")
	_ = stdout
	if exit == int(output.ExitOK) {
		t.Fatal("expected error")
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(errb), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, errb)
	}
	if res.Error.Code != "notfound.repo" {
		t.Errorf("code = %q, want notfound.repo", res.Error.Code)
	}
	if exit != int(output.ExitNotFound) {
		t.Errorf("exit = %d, want ExitNotFound(%d)", exit, output.ExitNotFound)
	}
}
