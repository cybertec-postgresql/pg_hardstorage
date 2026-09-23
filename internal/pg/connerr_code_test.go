package pg

import (
	"errors"
	"net"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// A PostgreSQL connection failure must say PostgreSQL. It was coded
// storage.unreachable, so "the database is in crash recovery" reached
// whoever owns the storage backend — and doctor reported the same
// condition as pg.unreachable, giving one outage two names. The exit
// code (8, transient) is deliberately unchanged; only the code that
// alerting routes on is corrected.
func TestConnectFailureIsPGUnreachable(t *testing.T) {
	err := classifyConnectError(&net.OpError{Op: "dial", Net: "tcp",
		Err: errors.New("connect: connection refused")})

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("not a structured error: %v", err)
	}
	if oe.Code != "pg.unreachable" {
		t.Errorf("code = %q, want pg.unreachable — a PG connect failure is not a storage outage", oe.Code)
	}
	if got := output.ExitCodeFor(err); got != output.ExitUnreachable {
		t.Errorf("exit = %d, want %d (unchanged: transient, retry)", got, output.ExitUnreachable)
	}
}
