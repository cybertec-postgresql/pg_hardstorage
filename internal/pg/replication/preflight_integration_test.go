// Build-tagged integration tests for the WAL-stream preflight
// against a real PG container.  Run via `make test-integration`.
//
//go:build integration

package replication_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
)

// findFinding returns the first finding with the given code, or
// nil if none.  Helper for the per-check assertions below.
func findFinding(res *replication.PreflightResult, code string) *replication.PreflightFinding {
	for i := range res.Findings {
		if res.Findings[i].Code == code {
			return &res.Findings[i]
		}
	}
	return nil
}

// TestIntegration_Preflight_HappyPath: a default postgres:17
// container has wal_level=replica, max_replication_slots=10,
// max_wal_senders=10, no max_slot_wal_keep_size, etc.  Preflight
// should produce zero fatal findings.
func TestIntegration_Preflight_HappyPath(t *testing.T) {
	srv := testkit.StartPostgres(t)
	reg := connectRegular(t, srv.DSN)

	// The testkit's container runs as the `hsctl` bootstrap
	// superuser (see testkit.StartPostgres → tcpostgres.WithUsername),
	// which has the REPLICATION attribute by default.  We
	// deliberately don't probe `postgres`: that role exists in
	// the cluster but isn't the bootstrap user, so it doesn't
	// auto-get REPLICATION.
	res, err := replication.Preflight(context.Background(), reg, "hsctl", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if res.HasFatal() {
		t.Errorf("expected no fatal findings on default config; got %+v", res.Findings)
	}
	if res.PgVersionNum < 150000 {
		t.Errorf("PgVersionNum = %d, want >= 150000 (PG 15+)", res.PgVersionNum)
	}
}

// TestIntegration_Preflight_RoleWithoutReplication: probing a role
// that doesn't have rolreplication=true must surface a fatal
// finding with code role.no_replication.  Catches the most common
// production misstep — the operator forgot to grant REPLICATION
// to the streamer's role.
func TestIntegration_Preflight_RoleWithoutReplication(t *testing.T) {
	srv := testkit.StartPostgres(t)
	reg := connectRegular(t, srv.DSN)

	// Create a non-replication role to probe.
	if _, err := reg.PgConn().Exec(context.Background(),
		`CREATE ROLE preflight_probe LOGIN PASSWORD 'x'`).ReadAll(); err != nil {
		t.Fatalf("create probe role: %v", err)
	}
	t.Cleanup(func() {
		reg2 := connectRegular(t, srv.DSN)
		_, _ = reg2.PgConn().Exec(context.Background(),
			`DROP ROLE IF EXISTS preflight_probe`).ReadAll()
		_ = reg2.Close(context.Background())
	})

	res, err := replication.Preflight(context.Background(), reg, "preflight_probe", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	f := findFinding(res, "role.no_replication")
	if f == nil {
		t.Fatalf("expected a role.no_replication finding; got %+v", res.Findings)
	}
	if f.Severity != replication.PreflightFatal {
		t.Errorf("role.no_replication should be fatal; got %s", f.Severity)
	}
	if !strings.Contains(f.Suggestion, "ALTER ROLE") {
		t.Errorf("role.no_replication suggestion should hint at ALTER ROLE; got %q", f.Suggestion)
	}
}

// TestIntegration_Preflight_RoleSkippedWhenEmpty: passing an empty
// connectingRole skips the role probe.  Confirms operators who
// don't supply --role still get the rest of the checks without a
// false-positive on the role check.
func TestIntegration_Preflight_RoleSkippedWhenEmpty(t *testing.T) {
	srv := testkit.StartPostgres(t)
	reg := connectRegular(t, srv.DSN)

	res, err := replication.Preflight(context.Background(), reg, "", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if findFinding(res, "role.no_replication") != nil {
		t.Errorf("role.no_replication should not surface when role is empty; got %+v", res.Findings)
	}
}

// TestIntegration_Preflight_WALKeepSizeInfo: wal_keep_size
// surfaces as an info finding regardless of value.  Confirms the
// information-only path is wired up.
func TestIntegration_Preflight_WALKeepSizeInfo(t *testing.T) {
	srv := testkit.StartPostgres(t)
	reg := connectRegular(t, srv.DSN)

	res, err := replication.Preflight(context.Background(), reg, "", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	f := findFinding(res, "wal_keep_size")
	if f == nil {
		t.Fatalf("expected a wal_keep_size info finding; got %+v", res.Findings)
	}
	if f.Severity != replication.PreflightInfo {
		t.Errorf("wal_keep_size should be info; got %s", f.Severity)
	}
}
