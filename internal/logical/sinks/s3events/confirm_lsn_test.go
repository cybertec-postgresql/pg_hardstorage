package s3events_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical/commitlsn"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical/sinks/s3events"
)

// A stored batch that ends mid-transaction must not confirm the
// synthetic WALStart+len(Data) of its last row: that overshoots the
// transaction's own (not yet received) commit, and PostgreSQL skips
// the transaction after a restart. Only commit end LSNs are confirmed.
func TestSyncedLSN_ConfirmsOnlyCommitEndLSNs(t *testing.T) {
	sp, _ := freshStorage(t)
	sink, _ := s3events.New(s3events.Options{
		Storage: sp, Deployment: "db1", Stream: "evt",
		BatchSize: 100, Now: fixedNow(),
	})
	defer sink.Close()
	ctx := context.Background()

	_ = sink.OnRecord(ctx, rec(0x1000, 0x1000, "B"+strings.Repeat("\x00", 20)))
	_ = sink.OnRecord(ctx, rec(0x1010, 0x1010, "I"+strings.Repeat("x", 8192)))
	if err := sink.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := sink.SyncedLSN(); got != 0 {
		t.Fatalf("SyncedLSN = %s after a mid-transaction batch; want 0 (no commit stored)", got)
	}

	_ = sink.OnRecord(ctx, rec(0x1100, 0x1100, string(commitlsn.Message(0x1100))))
	if err := sink.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := sink.SyncedLSN(), pglogrepl.LSN(0x1100); got != want {
		t.Fatalf("SyncedLSN = %s, want the commit end LSN %s", got, want)
	}
}
