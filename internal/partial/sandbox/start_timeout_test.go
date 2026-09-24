package sandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/partial/sandbox"
)

// Regression (M107): `pg_ctl -w start` can fail (e.g. "server did not
// start in time") while the postmaster it forked is alive and holding
// the data dir. Start's failure path only removed the socket dir and
// restored auto.conf, leaking a running postmaster on the operator's
// restored data. It must issue a `pg_ctl stop` before giving up.
func TestStart_FailedStartStopsForkedPostmaster(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stubDir := t.TempDir()
	calls := filepath.Join(stubDir, "calls.log")
	stub := filepath.Join(stubDir, "pg_ctl")
	script := "#!/bin/sh\n" +
		"echo \"$1\" >> '" + calls + "'\n" +
		"case \"$1\" in start) exit 1;; esac\n" +
		"exit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := sandbox.Start(context.Background(), sandbox.Options{
		DataDir:          dataDir,
		PGCtlPath:        stub,
		PGDumpPath:       "/bin/false",
		SkipVersionCheck: true,
	})
	if err == nil {
		t.Fatal("expected Start to fail")
	}
	body, rerr := os.ReadFile(calls)
	if rerr != nil {
		t.Fatal(rerr)
	}
	lines := strings.Fields(string(body))
	if len(lines) < 2 || lines[0] != "start" || lines[len(lines)-1] != "stop" {
		t.Fatalf("pg_ctl calls = %q; want start followed by stop", lines)
	}
}
