//go:build integration

package replication_test

// Preflight against a real PostgreSQL started from the local binaries
// (initdb / pg_ctl on PATH — e.g. export PATH=/usr/lib/postgresql/17/bin:$PATH).
// No Docker: skipped when the binaries are absent.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
)

// startLocalPG initdb's a throwaway cluster, starts it on a unix socket
// with extraConf, and returns a superuser DSN.
func startLocalPG(t *testing.T, extraConf ...string) string {
	t.Helper()
	initdb, err := exec.LookPath("initdb")
	if err != nil {
		t.Skip("initdb not on PATH (export PATH=/usr/lib/postgresql/17/bin:$PATH)")
	}
	pgctl := filepath.Join(filepath.Dir(initdb), "pg_ctl")
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	sock, err := os.MkdirTemp("", "pgsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sock) })
	if out, err := exec.Command(initdb, "-D", data, "-U", "postgres", "--auth=trust", "-N").CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, out)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	opts := fmt.Sprintf("-p %d -k %s -c listen_addresses=''", port, sock)
	for _, c := range extraConf {
		opts += " -c " + c
	}
	if out, err := exec.Command(pgctl, "-D", data, "-o", opts, "-l", filepath.Join(dir, "log"), "-w", "start").CombinedOutput(); err != nil {
		t.Fatalf("pg_ctl start: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(pgctl, "-D", data, "-m", "immediate", "stop").Run() })
	return fmt.Sprintf("host=%s port=%d user=postgres dbname=postgres sslmode=disable", sock, port)
}

func exec1(t *testing.T, c *pg.Conn, sql string) {
	t.Helper()
	if res := c.PgConn().Exec(context.Background(), sql); true {
		if _, err := res.ReadAll(); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

func codes(res *replication.PreflightResult) map[string]replication.PreflightSeverity {
	m := map[string]replication.PreflightSeverity{}
	for _, f := range res.Findings {
		m[f.Code] = f.Severity
	}
	return m
}

// H29: the streamer's own existing slot fills the table → must not be fatal.
// M62: a superuser without the REPLICATION attribute may replicate.
func TestIntegration_LocalPG_PreflightOwnSlotAndSuperuser(t *testing.T) {
	dsn := startLocalPG(t, "max_replication_slots=1", "wal_level=replica")
	ctx := context.Background()
	c, err := pg.Connect(ctx, dsn, pg.ModeRegular)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)

	exec1(t, c, `SELECT pg_create_physical_replication_slot('pg_hardstorage_db1')`)
	exec1(t, c, `CREATE ROLE boss SUPERUSER LOGIN NOREPLICATION`)

	res, err := replication.Preflight(ctx, c, "boss", "pg_hardstorage_db1")
	if err != nil {
		t.Fatal(err)
	}
	got := codes(res)
	if _, bad := got["max_replication_slots.full"]; bad {
		t.Errorf("own existing slot reported as a full slot table — wal stream could never restart: %+v", res.Findings)
	}
	if _, bad := got["role.no_replication"]; bad {
		t.Errorf("superuser refused for lacking REPLICATION; PostgreSQL admits superusers: %+v", res.Findings)
	}

	// A different streamer that must CREATE its slot is still refused.
	res, err = replication.Preflight(ctx, c, "boss", "pg_hardstorage_other")
	if err != nil {
		t.Fatal(err)
	}
	if codes(res)["max_replication_slots.full"] != replication.PreflightFatal {
		t.Errorf("full slot table not fatal for a streamer that must create a slot: %+v", res.Findings)
	}

	// A plain role without either attribute is still refused.
	exec1(t, c, `CREATE ROLE pleb LOGIN`)
	res, err = replication.Preflight(ctx, c, "pleb", "pg_hardstorage_db1")
	if err != nil {
		t.Fatal(err)
	}
	if codes(res)["role.no_replication"] != replication.PreflightFatal {
		t.Errorf("non-replication, non-superuser role not refused: %+v", res.Findings)
	}
}
