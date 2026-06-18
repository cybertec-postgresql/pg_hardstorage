// Integration tests against a real PostgreSQL container. Build-tagged
// so the default `go test ./...` skips them when Docker is not running.
//
// Run with:
//
//	make test-integration
//	go test -tags=integration -count=1 ./internal/pg/...
//
//go:build integration

package pg_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
)

func TestIntegration_Connect_Regular(t *testing.T) {
	pgsrv := testkit.StartPostgres(t)

	ctx := context.Background()
	c, err := pg.Connect(ctx, pgsrv.DSN, pg.ModeRegular)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close(ctx)

	if err := c.Ping(ctx); err != nil {
		t.Errorf("ping: %v", err)
	}
}

func TestIntegration_QueryVersion(t *testing.T) {
	pgsrv := testkit.StartPostgres(t)

	ctx := context.Background()
	c, err := pg.Connect(ctx, pgsrv.DSN, pg.ModeRegular)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)

	v, err := pg.QueryVersion(ctx, c)
	if err != nil {
		t.Fatalf("QueryVersion: %v", err)
	}
	if want := testkit.ExpectedPGMajorInt(); v.Major != want {
		t.Errorf("major = %d, want %d", v.Major, want)
	}
	if !v.AtLeast(15, 0) {
		t.Errorf("AtLeast(15, 0) should be true; got version %+v", v)
	}
}

func TestIntegration_Connect_Replication(t *testing.T) {
	pgsrv := testkit.StartPostgres(t)

	ctx := context.Background()
	c, err := pg.Connect(ctx, pgsrv.DSN, pg.ModeReplication)
	if err != nil {
		t.Fatalf("connect (replication): %v", err)
	}
	defer c.Close(ctx)

	if err := c.Ping(ctx); err != nil {
		t.Errorf("ping replication conn: %v", err)
	}
	if c.Mode() != pg.ModeReplication {
		t.Errorf("Mode() = %s, want replication", c.Mode())
	}
}

func TestIntegration_Connect_BadDSN_UsageError(t *testing.T) {
	// No Docker required for this — pg.ParseConfig fails locally.
	_, err := pg.Connect(context.Background(), "this-is-not-a-dsn://", pg.ModeRegular)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !errors.Is(err, output.ErrUsage) {
		t.Errorf("bad DSN should map to a usage error; got %v", err)
	}
}

func TestIntegration_QueryVersion_RejectsReplicationMode(t *testing.T) {
	pgsrv := testkit.StartPostgres(t)

	ctx := context.Background()
	c, err := pg.Connect(ctx, pgsrv.DSN, pg.ModeReplication)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)

	if _, err := pg.QueryVersion(ctx, c); err == nil {
		t.Error("QueryVersion against a replication-mode conn must error")
	}
}

// TestIntegration_TimelineHistory_TLI1ReturnsSentinel: a fresh PG
// container is on TLI 1; PG returns an empty result for
// TIMELINE_HISTORY 1 (no parent timeline). We surface this as the
// ErrNoHistoryForTLI1 sentinel so the leader-follow loop can call
// it on every reconnect without triggering a false alarm on the
// initial timeline.
func TestIntegration_TimelineHistory_TLI1ReturnsSentinel(t *testing.T) {
	pgsrv := testkit.StartPostgres(t)

	ctx := context.Background()
	c, err := pg.Connect(ctx, pgsrv.DSN, pg.ModeReplication)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)

	_, err = pg.TimelineHistoryFor(ctx, c, 1)
	if !errors.Is(err, pg.ErrNoHistoryForTLI1) {
		t.Errorf("TLI 1 should return ErrNoHistoryForTLI1; got %v", err)
	}
}

// TestIntegration_TimelineHistory_RejectsRegularMode: TIMELINE_HISTORY
// is a replication-mode-only command; the helper refuses regular
// mode with usage.wrong_mode.
func TestIntegration_TimelineHistory_RejectsRegularMode(t *testing.T) {
	pgsrv := testkit.StartPostgres(t)

	ctx := context.Background()
	c, err := pg.Connect(ctx, pgsrv.DSN, pg.ModeRegular)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)

	_, err = pg.TimelineHistoryFor(ctx, c, 2)
	if err == nil {
		t.Fatal("expected wrong-mode error")
	}
	if !errors.Is(err, output.ErrUsage) {
		t.Errorf("wrong-mode error should be ExitMisuse; got %v", err)
	}
}

// TestIntegration_TimelineHistory_RejectsZeroTLI: a zero TLI is a
// misuse; the helper refuses BEFORE issuing the wire query so the
// operator sees a clear error rather than PG's own diagnostic.
func TestIntegration_TimelineHistory_RejectsZeroTLI(t *testing.T) {
	pgsrv := testkit.StartPostgres(t)

	ctx := context.Background()
	c, err := pg.Connect(ctx, pgsrv.DSN, pg.ModeReplication)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)

	_, err = pg.TimelineHistoryFor(ctx, c, 0)
	if err == nil {
		t.Fatal("expected zero-TLI error")
	}
	if !errors.Is(err, output.ErrUsage) {
		t.Errorf("zero TLI should map to ExitMisuse; got %v", err)
	}
}
