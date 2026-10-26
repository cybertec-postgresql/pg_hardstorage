package config_test

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

// Issue #75: the scheduled backup's --include-wal / --fast / --(no-)encrypt
// are configurable per deployment.
func TestLoad_DeploymentBackupOptions(t *testing.T) {
	p := pathsForTempDir(t, t.TempDir())
	t.Setenv("PG_HARDSTORAGE_CONFIG", `
deployments:
  a:
    pg_connection: postgres://x@h/db
    repo: file:///r
    backup: { include_wal: true, fast: true, encrypt: false }
  b:
    pg_connection: postgres://x@h/db
    repo: file:///r
`)
	res, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := res.Config.Deployments["a"].Backup
	if !a.IncludeWAL || !a.Fast || a.Encrypt == nil || *a.Encrypt {
		t.Errorf("a.Backup = %+v, want include_wal, fast, encrypt=false", a)
	}
	if b := res.Config.Deployments["b"].Backup; b.IncludeWAL || b.Fast || b.Encrypt != nil {
		t.Errorf("b.Backup = %+v, want zero value", b)
	}
}
