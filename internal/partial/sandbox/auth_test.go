package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The sandbox is a cluster restored from the operator's backup. Two
// things follow that the original code got wrong in the same breath:
//
//  1. Its ROLES are the source cluster's roles. The host's login name
//     is unrelated to them, so connecting as $USER worked only when
//     the operator's login happened to match a role in their own
//     backup. Everywhere else: FATAL: role "<login>" does not exist.
//
//  2. Its pg_hba.conf is the operator's production auth POLICY. It may
//     demand scram, LDAP, GSSAPI or a client certificate — none of
//     which we can present, because we are not the application. Even
//     `peer` only works by the coincidence in (1).
//
// So the sandbox supplies its own trust-only pg_hba and connects as a
// named role. Trust is confined to a cluster with no TCP listener
// whose socket lives in a 0700 directory this process just created
// and will delete.

// TestSandboxHBAIsTrustOnly pins the generated rules. A future edit
// that "hardens" this to peer or scram reintroduces the failure for
// every cluster whose superuser is not the local login.
func TestSandboxHBAIsTrustOnly(t *testing.T) {
	if !strings.Contains(sandboxHBA, "local") || !strings.Contains(sandboxHBA, "trust") {
		t.Fatalf("the sandbox must define a local trust rule, got:\n%s", sandboxHBA)
	}
	for _, method := range []string{"peer", "scram-sha-256", "md5", "ldap", "cert", "gss"} {
		if strings.Contains(sandboxHBA, method) {
			t.Errorf("sandbox pg_hba must not require %q — we hold no credential for the "+
				"restored cluster and cannot satisfy it", method)
		}
	}
	// host lines would mean a TCP listener, which the sandbox
	// deliberately does not have (listen_addresses = '').
	for _, line := range strings.Split(sandboxHBA, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "host") {
			t.Errorf("sandbox pg_hba must not open a host (TCP) rule: %q", line)
		}
	}
}

// TestSandboxAutoConfRedirectsHBA: the trust rules only take effect if
// PG is pointed at them. Redirecting hba_file (rather than rewriting
// the data dir's pg_hba.conf) is also what keeps the operator's own
// file untouched, so there is nothing to restore on Stop and nothing
// left behind if the process dies mid-run.
func TestSandboxAutoConfRedirectsHBA(t *testing.T) {
	sock := "/tmp/pg_hardstorage-sandbox-sock-test"
	conf := string(buildSandboxAutoConf(nil, sock))

	want := "hba_file = '" + filepath.Join(sock, "pg_hba.conf") + "'"
	if !strings.Contains(conf, want) {
		t.Errorf("auto.conf must redirect hba_file into the sandbox socket dir.\nwant: %s\ngot:\n%s", want, conf)
	}
	// The socket settings must survive alongside it.
	for _, must := range []string{"listen_addresses = ''", "unix_socket_directories = '" + sock + "'"} {
		if !strings.Contains(conf, must) {
			t.Errorf("auto.conf lost %q:\n%s", must, conf)
		}
	}
}

// TestSandboxAutoConfPreservesExisting guards issue #96's fix while
// the hba_file line is added next to it: the restore writes a
// restore_command and standby.signal into this same file, and
// replacing rather than appending left PG in a recovery that never
// finished.
func TestSandboxAutoConfPreservesExisting(t *testing.T) {
	existing := []byte("restore_command = '/usr/bin/pg_hardstorage wal fetch %f %p'\n")
	conf := string(buildSandboxAutoConf(existing, "/tmp/sock"))
	if !strings.Contains(conf, "restore_command") {
		t.Fatal("appending the sandbox overrides must not drop the restore's own settings (issue #96)")
	}
	if strings.Index(conf, "restore_command") > strings.Index(conf, "hba_file") {
		t.Error("the overrides must come AFTER the existing content; auto.conf is last-wins")
	}
}

// TestDefaultSandboxUserIsNotTheHostLogin is the direct regression.
// The default must be a property of PostgreSQL (what initdb creates),
// never of whoever is running the command.
func TestDefaultSandboxUserIsNotTheHostLogin(t *testing.T) {
	t.Setenv("USER", "some-random-operator-login")
	t.Setenv("LOGNAME", "some-random-operator-login")

	if DefaultSandboxUser != "postgres" {
		t.Errorf("DefaultSandboxUser = %q, want \"postgres\" (what initdb creates absent -U)", DefaultSandboxUser)
	}
	if DefaultSandboxUser == os.Getenv("USER") {
		t.Error("the sandbox role must not follow the host login; that is the bug this replaced")
	}
}

// TestIsRoleMissing separates "this cluster has no such role" — which
// --pg-user answers — from every other reason pg_dump exits 1.
func TestIsRoleMissing(t *testing.T) {
	cases := []struct {
		stderr string
		want   bool
	}{
		{`pg_dump: error: connection to server on socket "/tmp/x/.s.PGSQL.5432" failed: FATAL:  role "postgres" does not exist`, true},
		{`FATAL:  role "cybertec" does not exist`, true},
		{`pg_dump: error: no matching tables were found`, false},
		{`pg_dump: error: connection to server failed: FATAL:  database "app" does not exist`, false},
		{`pg_dump: error: query failed: ERROR:  permission denied for table accounts`, false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isRoleMissing(tc.stderr); got != tc.want {
			t.Errorf("isRoleMissing(%q) = %v, want %v", tc.stderr, got, tc.want)
		}
	}
}

// TestErrRoleMissingIsIdentifiable — the CLI routes on errors.Is to
// raise preflight.pg_role_missing with the --pg-user advice.
func TestErrRoleMissingIsIdentifiable(t *testing.T) {
	wrapped := fmt.Errorf("%w: %s", ErrRoleMissing, `FATAL:  role "postgres" does not exist`)
	if !errors.Is(wrapped, ErrRoleMissing) {
		t.Fatal("wrapping broke errors.Is")
	}
	if errors.Is(wrapped, ErrNoMatchingTables) {
		t.Fatal("a missing role must not be confused with an empty table match")
	}
}
