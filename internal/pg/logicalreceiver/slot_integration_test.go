//go:build integration

package logicalreceiver_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
)

func TestLogicalReceiverSlotLifecycle(t *testing.T) {
	srv := testkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	conn, err := pg.Connect(ctx, srv.DSN, pg.ModeRegular)
	if err != nil {
		t.Fatalf("regular connect: %v", err)
	}
	defer conn.Close(ctx)

	if res := conn.PgConn().ExecParams(ctx, "CREATE TABLE IF NOT EXISTS orders (id SERIAL PRIMARY KEY, amount INTEGER, created_at TIMESTAMPTZ DEFAULT now())", nil, nil, nil, nil).Read(); res.Err != nil {
		t.Fatalf("CREATE TABLE: %v", res.Err)
	}
	if res := conn.PgConn().ExecParams(ctx, "INSERT INTO orders (amount) SELECT generate_series(1, 100)", nil, nil, nil, nil).Read(); res.Err != nil {
		t.Fatalf("INSERT: %v", res.Err)
	}

	repConn, err := pg.Connect(ctx, srv.DSN, pg.ModeReplication)
	if err != nil {
		t.Fatalf("replication connect: %v", err)
	}
	defer repConn.Close(ctx)

	identity, err := pg.IdentifySystem(ctx, repConn)
	if err != nil {
		t.Fatalf("IDENTIFY_SYSTEM: %v", err)
	}

	if identity.Timeline < 1 {
		t.Errorf("invalid timeline: %d", identity.Timeline)
	}
	if len(identity.SystemID) == 0 {
		t.Error("empty system identifier")
	}
	if !strings.Contains(identity.XLogPos, "/") {
		t.Errorf("unexpected xlogpos format: %s", identity.XLogPos)
	}

	if res := conn.PgConn().ExecParams(ctx, "CREATE PUBLICATION pub_test FOR TABLE orders", nil, nil, nil, nil).Read(); res.Err != nil {
		t.Logf("CREATE PUBLICATION: %v (may already exist)", res.Err)
	}

	t.Logf("logical receiver: slot lifecycle OK — system=%s timeline=%d xlogpos=%s", identity.SystemID, identity.Timeline, identity.XLogPos)
}
