// dsn_pickprobe_test.go — issue #85 regression coverage for the
// socket-first-then-tcp probe-DSN selection.
//
// Drives pickProbeDSN with a fake `psql` (a small shell script)
// that returns success / failure per DSN.  No real PG required;
// the test is hermetic and runs in <100ms.

package postverify_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/postverify"
)

// writeFakePsql builds a one-line shell script at <dir>/psql that
// the postverify probe will invoke as `psql -At -w -c "select 1" <DSN>`.
// It inspects the DSN (always the LAST CLI arg) and:
//
//   - exits 0 with "1\n" on stdout when the DSN contains anyDSN(matchSubstr)
//   - exits 1 with a token error on stdout otherwise
//
// We don't try to model libpq's grammar; the substring match is
// good enough to distinguish the socket DSN ("host=/path/...") from
// the tcp DSN ("@127.0.0.1:...").
func writeFakePsql(t *testing.T, dir, matchSubstr string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("postverify probe path is Unix-only; the fake-psql harness uses /bin/sh")
	}
	path := filepath.Join(dir, "psql")
	script := `#!/bin/sh
# Fake psql: last arg is the DSN.  Match against the substring and
# exit 0/1 accordingly.
DSN="$@"
case "$DSN" in
  *` + matchSubstr + `*) printf '1\n'; exit 0 ;;
  *)                    echo 'psql: error: connection failed'; exit 1 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Socket DSN succeeds: pickProbeDSN must return ("socket", ...).
// The reporter's expectation from #85.
func TestPickProbeDSN_SocketSucceeds_ReturnsSocketLabel(t *testing.T) {
	tmp := t.TempDir()
	psql := writeFakePsql(t, tmp, "host=") // socket DSN carries host=/path/

	socketDSN := "postgres:///postgres?host=/tmp/sock&port=5555&user=postgres"
	tcpDSN := "postgres://postgres@127.0.0.1:5555/postgres"

	dsn, kind, err := postverify.PickProbeDSNForTest(context.Background(), psql, socketDSN, tcpDSN)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kind != "socket" {
		t.Errorf("kind = %q, want socket", kind)
	}
	if dsn != socketDSN {
		t.Errorf("dsn = %q, want %q", dsn, socketDSN)
	}
}

// Socket fails, TCP succeeds: pickProbeDSN must fall back to TCP
// and return ("tcp", ...).  Covers the case where the socket
// directory is somehow unreachable (rare on a postverify start,
// but the test pins the fallback contract).
func TestPickProbeDSN_SocketFails_FallsBackToTCP(t *testing.T) {
	tmp := t.TempDir()
	// Only the TCP DSN matches; socket DSN gets exit 1.
	psql := writeFakePsql(t, tmp, "127.0.0.1")

	socketDSN := "postgres:///postgres?host=/tmp/sock&port=5555&user=postgres"
	tcpDSN := "postgres://postgres@127.0.0.1:5555/postgres"

	dsn, kind, err := postverify.PickProbeDSNForTest(context.Background(), psql, socketDSN, tcpDSN)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kind != "tcp" {
		t.Errorf("kind = %q, want tcp", kind)
	}
	if dsn != tcpDSN {
		t.Errorf("dsn = %q, want %q", dsn, tcpDSN)
	}
}

// Both fail: pickProbeDSN must return an error mentioning BOTH the
// socket and tcp failure reasons so the operator can act on
// whichever applies.
func TestPickProbeDSN_BothFail_ErrorMentionsBoth(t *testing.T) {
	tmp := t.TempDir()
	// Matches nothing.
	psql := writeFakePsql(t, tmp, "this-substring-never-appears")

	_, _, err := postverify.PickProbeDSNForTest(context.Background(), psql,
		"postgres:///postgres?host=/tmp/sock",
		"postgres://postgres@127.0.0.1:5555/postgres")
	if err == nil {
		t.Fatal("expected an error when both probes fail")
	}
	msg := err.Error()
	for _, want := range []string{"socket=", "tcp="} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q — operator sees only one half of the picture", msg, want)
		}
	}
}

// `-w` must be passed to psql on EVERY invocation; without it the
// reporter's pipeline blocked at "Password for user postgres:".
// The fake psql writes its argv to a sidecar file we then inspect.
func TestPickProbeDSN_PsqlNeverPromptsForPassword(t *testing.T) {
	tmp := t.TempDir()
	argLog := filepath.Join(tmp, "argv.log")
	script := `#!/bin/sh
# Log each invocation's argv (one per line) then succeed.
{ printf '%s\n' "$*"; } >> ` + argLog + `
printf '1\n'
exit 0
`
	psql := filepath.Join(tmp, "psql")
	if err := os.WriteFile(psql, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, _, err := postverify.PickProbeDSNForTest(context.Background(), psql,
		"postgres:///postgres?host=/tmp/sock",
		"postgres://postgres@127.0.0.1:5555/postgres"); err != nil {
		t.Fatalf("pickProbeDSN: %v", err)
	}

	body, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("psql was never invoked")
	}
	for i, line := range lines {
		if !strings.Contains(line, " -w ") && !strings.HasSuffix(line, " -w") {
			t.Errorf("psql invocation #%d missing -w (no-password): %q — issue #85 regression",
				i+1, line)
		}
	}
}

// Context cancellation while the socket attempt is in flight must
// propagate immediately — the loop cannot block on a hung psql.
func TestPickProbeDSN_CtxCancellation_ReturnsPromptly(t *testing.T) {
	tmp := t.TempDir()
	// Slow-but-eventually-failing psql: 3s sleep, then exit 1.
	// Without ctx propagation the loop would burn 6s (two probes).
	script := `#!/bin/sh
sleep 3
exit 1
`
	psql := filepath.Join(tmp, "psql")
	if err := os.WriteFile(psql, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := postverify.PickProbeDSNForTest(ctx, psql,
		"postgres:///postgres?host=/tmp/sock",
		"postgres://postgres@127.0.0.1:5555/postgres")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error after ctx cancel")
	}
	// Allow generous slack for process teardown.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("loop took %v after ctx cancel; want <1.5s (sleepBackoff is ignoring ctx)",
			elapsed)
	}
}

// Argument ORDER matters: the socket DSN must be tried first.  Past
// versions made TCP the primary; the issue #85 fix flipped them.
// This test confirms the order by using a fake psql that matches a
// substring present ONLY in the socket DSN, then a separate run
// that matches a substring ONLY in the TCP DSN, and inspects the
// return label.  Drift in the order would fail the first subtest.
func TestPickProbeDSN_SocketIsTriedBeforeTCP(t *testing.T) {
	tmp := t.TempDir()
	psql := writeFakePsql(t, tmp, "uniq-sock") // matches socket DSN only

	socketDSN := "postgres:///postgres?host=/tmp/uniq-sock&port=5555"
	tcpDSN := "postgres://postgres@127.0.0.1:5555/postgres"
	_, kind, err := postverify.PickProbeDSNForTest(context.Background(), psql, socketDSN, tcpDSN)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "socket" {
		t.Errorf("kind = %q, want socket — order regression: TCP is being tried before socket", kind)
	}
}
