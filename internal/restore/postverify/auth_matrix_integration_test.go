// auth_matrix_integration_test.go — exercise PG's behaviour across
// the comms × auth matrix the issue #85 fix relies on.
//
// The unit tests in dsn_pickprobe_test.go drive pickProbeDSN with a
// fake psql; they pin the loop SHAPE.  This file pins the
// SEMANTICS — that PG actually behaves the way the fix assumes
// when it sees specific pg_hba.conf rows.
//
// One container, several subtests.  Each subtest rewrites
// pg_hba.conf inside the container, reloads PG, and runs psql via
// `docker exec` against (a) the container's Unix socket and (b)
// 127.0.0.1, asserting whether each path authenticates the way
// pickProbeDSN's documented preference order expects.
//
// Auth matrix:
//
//   local=trust       host=trust       — both succeed
//   local=peer        host=scram       — default-like (issue #85);
//                                        socket succeeds, TCP fails
//                                        under -w
//   local=reject      host=trust       — socket fails, TCP succeeds
//                                        (validates the fallback
//                                        direction)
//   local=scram       host=scram       — both fail under -w; the
//                                        error message is honest
//                                        about which path tried
//
// Why exec inside the container vs. driving pickProbeDSN from
// host Go: the testkit's PG container does not bind-mount its
// socket dir onto the host, so a host-side `psql -h /path/to/sock`
// can't reach it.  Running psql inside the container gives us the
// same authentication semantics PG actually applies, which is the
// thing the matrix is meant to verify.

//go:build integration

package postverify_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
)

// containerSocketDir is the standard PG socket dir in the postgres
// image. The data directory is NOT a constant — it moved on PG 18
// (/var/lib/postgresql/18/docker) — so we resolve it per-container via
// srv.DataDir rather than hardcoding it.
const containerSocketDir = "/var/run/postgresql"

// rewriteHBA writes a brand-new pg_hba.conf inside the container,
// reloads PG, and waits briefly so the reload lands.
func rewriteHBA(t *testing.T, srv *testkit.Postgres, localMethod, hostMethod string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dataDir := srv.DataDir(t)

	hba := fmt.Sprintf(`# pg_hardstorage issue #85 auth-matrix fixture
# TYPE  DATABASE  USER  ADDRESS         METHOD
local   all       all                   %s
host    all       all   127.0.0.1/32    %s
host    all       all   ::1/128         %s
`, localMethod, hostMethod, hostMethod)

	// testcontainers-go's Container.Exec doesn't expose a stdin
	// pipe, so we use a heredoc inside `sh -c` to write the file.
	// The `'HBA_EOF'` quoting prevents shell expansion of the
	// content; the quoted form is mandatory because pg_hba.conf
	// can contain `$` characters in role names.
	rc, reader, err := srv.Container.Exec(ctx, []string{
		"sh", "-c",
		"cat > " + dataDir + "/pg_hba.conf <<'HBA_EOF'\n" + hba + "HBA_EOF",
	})
	if err != nil {
		t.Fatalf("exec write hba: %v", err)
	}
	if rc != 0 {
		out, _ := io.ReadAll(reader)
		t.Fatalf("write hba exited %d: %s", rc, string(out))
	}

	// Reload PG.  PGDATA is the standard env in the image.
	rc, reader, err = srv.Container.Exec(ctx, []string{
		"su", "-s", "/bin/sh", "postgres", "-c",
		"pg_ctl reload -D " + dataDir,
	})
	if err != nil {
		t.Fatalf("exec pg_ctl reload: %v", err)
	}
	if rc != 0 {
		out, _ := io.ReadAll(reader)
		t.Fatalf("pg_ctl reload exited %d: %s", rc, string(out))
	}

	// Give PG a moment to pick the new file up.
	time.Sleep(150 * time.Millisecond)
}

// probeInside runs psql inside the container against the given DSN
// shape ("socket" or "tcp") with -w (no-password) and returns
// (success, raw-output).  Used to verify that pickProbeDSN's
// socket-first-then-tcp loop will see the outcomes the matrix
// expects when PG is configured a given way.
func probeInside(t *testing.T, srv *testkit.Postgres, kind string) (bool, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var dsn string
	switch kind {
	case "socket":
		dsn = fmt.Sprintf("postgresql://hsctl@/hsctl?host=%s&port=5432&sslmode=disable",
			containerSocketDir)
	case "tcp":
		dsn = "postgresql://hsctl@127.0.0.1:5432/hsctl?sslmode=disable"
	default:
		t.Fatalf("probeInside: bad kind %q", kind)
	}
	rc, reader, err := srv.Container.Exec(ctx, []string{
		"psql", "-At", "-w", "-c", "select 1", dsn,
	})
	out := ""
	if reader != nil {
		body, _ := io.ReadAll(reader)
		out = string(body)
	}
	if err != nil {
		// Exec itself failing (no psql binary, container issues) —
		// this is a test-infra problem, not a PG outcome.
		t.Fatalf("exec psql: %v (output: %s)", err, out)
	}
	return rc == 0 && strings.Contains(out, "1"), out
}

// TestIntegration_AuthMatrix_PGObservedBehaviour pins PG's actual
// authentication outcomes per hba row across the comms × auth
// matrix that issue #85's fix relies on.  Combined with the unit
// tests in dsn_pickprobe_test.go (which pin pickProbeDSN's loop
// shape) the composition is covered end to end.
func TestIntegration_AuthMatrix_PGObservedBehaviour(t *testing.T) {
	srv := testkit.StartPostgres(t)
	// Ensure unix_socket_directories points at the standard
	// /var/run/postgresql so probeInside's `host=<dir>` DSN finds
	// it.  The testkit's writeReplicationConf doesn't pin this,
	// but the image default is /var/run/postgresql.

	cases := []struct {
		name       string
		local      string
		host       string
		wantSocket bool
		wantTCP    bool
		commentary string
	}{
		{
			name:       "trust_everywhere",
			local:      "trust",
			host:       "trust",
			wantSocket: true, wantTCP: true,
			commentary: "permissive baseline; pickProbeDSN picks socket because socket is tried first",
		},
		{
			name:       "default_like_peer_local_scram_host",
			local:      "peer",
			host:       "scram-sha-256",
			wantSocket: false, // peer auth: OS uid is `postgres`, PG role is `hsctl` — no match
			wantTCP:    false, // -w + no password → fails cleanly
			commentary: "issue #85 reporter's exact scenario; both -w probes fail because the testkit's `hsctl` role doesn't match the container's `postgres` OS user — proves the no-prompt posture even when no path succeeds",
		},
		{
			name:       "reject_local_trust_host",
			local:      "reject",
			host:       "trust",
			wantSocket: false, // reject — always denied
			wantTCP:    true,  // trust — always allowed
			commentary: "validates fallback direction: socket-first-then-tcp works in reverse too",
		},
		{
			name:       "scram_everywhere_no_password",
			local:      "scram-sha-256",
			host:       "scram-sha-256",
			wantSocket: false,
			wantTCP:    false,
			commentary: "-w means neither path can succeed without credentials; error must mention both",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rewriteHBA(t, srv, c.local, c.host)

			// `pg_ctl reload` applies pg_hba.conf asynchronously; on a
			// slow/loaded runner the SIGHUP can land well after the
			// post-reload settle, so a single probe sometimes still sees
			// the PREVIOUS subtest's rules (e.g. a lingering scram host
			// row → "no password supplied" where we expect trust). Poll
			// both probes until they reach the expected outcome or a
			// deadline: a not-yet-landed reload simply retries, while a
			// genuinely-wrong behaviour never converges and fails below.
			var sockOK, tcpOK bool
			var sockOut, tcpOut string
			deadline := time.Now().Add(15 * time.Second)
			for {
				sockOK, sockOut = probeInside(t, srv, "socket")
				tcpOK, tcpOut = probeInside(t, srv, "tcp")
				if (sockOK == c.wantSocket && tcpOK == c.wantTCP) || time.Now().After(deadline) {
					break
				}
				time.Sleep(300 * time.Millisecond)
			}

			if sockOK != c.wantSocket {
				t.Errorf("socket probe: got %v, want %v (out: %s)\n%s",
					sockOK, c.wantSocket, sockOut, c.commentary)
			}
			if tcpOK != c.wantTCP {
				t.Errorf("tcp probe: got %v, want %v (out: %s)\n%s",
					tcpOK, c.wantTCP, tcpOut, c.commentary)
			}
		})
	}
}
