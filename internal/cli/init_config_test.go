package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

func initTestPaths(t *testing.T, override string) *paths.Paths {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", dir)
	t.Setenv("PG_HARDSTORAGE_CONFIG", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_FILE", override)
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestWriteInitConfig_HonoursConfigFlag pins M49: `init -c <file>` read
// the named file but wrote the default one.
func TestWriteInitConfig_HonoursConfigFlag(t *testing.T) {
	override := filepath.Join(t.TempDir(), "staging.yaml")
	p := initTestPaths(t, override)
	if err := writeInitConfig(p, "db1", "postgres://u@h/db", "file:///tmp/r", "every 6h", "off"); err != nil {
		t.Fatalf("writeInitConfig: %v", err)
	}
	if body, err := os.ReadFile(override); err != nil || !strings.Contains(string(body), "db1:") {
		t.Errorf("-c file not written: %v\n%s", err, body)
	}
	if _, err := os.Stat(filepath.Join(p.Config.Value, "pg_hardstorage.yaml")); err == nil {
		t.Error("init wrote the default config file although -c named another")
	}
	if got := initConfigPath(p); got != override {
		t.Errorf("reported config path = %q, want %q", got, override)
	}
}

// TestWriteInitConfig_KeepsOtherLayersOut: init's write-back goes through
// the same layered edit as `deployment add`, so env-var YAML (and its
// credentials) is not flattened into the main file.
func TestWriteInitConfig_KeepsOtherLayersOut(t *testing.T) {
	p := initTestPaths(t, "")
	t.Setenv("PG_HARDSTORAGE_CONFIG", "deployments:\n  envdb:\n    pg_connection: \"host=h password=ENVSECRET\"\n    repo: file:///tmp/e\n")
	if err := writeInitConfig(p, "db1", "postgres://u@h/db", "file:///tmp/r", "every 6h", "off"); err != nil {
		t.Fatalf("writeInitConfig: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(p.Config.Value, "pg_hardstorage.yaml"))
	if !strings.Contains(string(body), "db1:") {
		t.Fatalf("db1 not written:\n%s", body)
	}
	if strings.Contains(string(body), "envdb") || strings.Contains(string(body), "ENVSECRET") {
		t.Errorf("env-var config flattened into the main file:\n%s", body)
	}
}

// TestInitResult_RedactsConnection pins M56: the result document (the
// JSON body scripts log) carried the raw pg_connection, password and all.
func TestInitResult_RedactsConnection(t *testing.T) {
	p := initTestPaths(t, "")
	for _, dsn := range []string{
		"host=h user=u password=hunter2 dbname=x",
		"postgres://u:hunter2@h/db",
	} {
		body := newInitResultBody(p, initOpts{}, "db1", dsn, "file:///tmp/r",
			17, pg.SystemIdentity{SystemID: "1", Timeline: 1}, nil, true, false)
		if strings.Contains(body.PGConnection, "hunter2") {
			t.Errorf("init result carries the password: %q", body.PGConnection)
		}
	}
}

// TestResolveQuickPGConn_SocketDirectory: a PGHOST naming a socket
// directory must yield a DSN whose host is that directory — it used to
// become postgres:///var/run/postgresql:5432/, i.e. database
// "var/run/postgresql:5432/" on the default host.
func TestResolveQuickPGConn_SocketDirectory(t *testing.T) {
	cases := []struct{ host, port, wantHost string }{
		{"/var/run/postgresql", "", "/var/run/postgresql"},
		{"/tmp", "5433", "/tmp"},
		{"db.example.com", "", "db.example.com"},
	}
	for _, c := range cases {
		t.Setenv("PGHOST", c.host)
		t.Setenv("PGPORT", c.port)
		dsn := resolveQuickPGConn()
		cfg, err := pgconn.ParseConfig(dsn)
		if err != nil {
			t.Errorf("PGHOST=%s: DSN %q does not parse: %v", c.host, dsn, err)
			continue
		}
		if cfg.Host != c.wantHost {
			t.Errorf("PGHOST=%s: DSN %q connects to host %q", c.host, dsn, cfg.Host)
		}
		if strings.Contains(cfg.Database, "/") || strings.Contains(cfg.Database, ":") {
			t.Errorf("PGHOST=%s: DSN %q names database %q", c.host, dsn, cfg.Database)
		}
		wantPort := uint16(5432)
		if c.port == "5433" {
			wantPort = 5433
		}
		if cfg.Port != wantPort {
			t.Errorf("PGHOST=%s: port %d, want %d", c.host, cfg.Port, wantPort)
		}
	}
}
