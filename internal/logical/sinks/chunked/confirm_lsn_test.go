package chunked_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical/commitlsn"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical/sinks/chunked"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/logicalreceiver"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// The sink confirmed WALStart+len(Data) of the last flushed record.
// A flush that lands mid-transaction on a large row therefore confirms
// an LSN past that transaction's (not yet received) commit; after a
// restart PostgreSQL skips every transaction whose commit lies below
// confirmed_flush_lsn, so it is lost from the archive. SyncedLSN must
// only ever be a commit end LSN from a durably-committed batch.
func TestSink_ConfirmsOnlyCommitEndLSNs(t *testing.T) {
	ctx := context.Background()
	sp := openFS(t, "file://"+t.TempDir())
	defer sp.Close()
	s, err := chunked.New(casdefault.New(sp), sp, chunked.Options{
		Deployment: "db1", StreamName: "events", Slot: "slot1",
		BatchBytes: 1 << 20, // only explicit Flush commits
	})
	if err != nil {
		t.Fatal(err)
	}

	// T1: BEGIN at 0x1000, one 8 KiB row at 0x1010, commit ends at
	// 0x1100. The row's synthetic end (0x1010+8192) lies far past
	// 0x1100 — and past T2's commit, which has not arrived.
	begin := logicalreceiver.Record{WALStart: 0x1000, Data: append([]byte{'B'}, make([]byte, 20)...)}
	row := logicalreceiver.Record{WALStart: 0x1010, Data: append([]byte{'I'}, bytes.Repeat([]byte("x"), 8192)...)}
	for _, r := range []logicalreceiver.Record{begin, row} {
		if err := s.OnRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// Mid-transaction flush (cadence tick or shutdown).
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := s.SyncedLSN(); got != 0 {
		t.Fatalf("SyncedLSN = %s after a mid-transaction flush; no commit was archived, "+
			"so confirming anything moves the slot past a commit not yet received", got)
	}

	commit := logicalreceiver.Record{WALStart: 0x1100, Data: commitlsn.Message(0x1100)}
	if err := s.OnRecord(ctx, commit); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := s.SyncedLSN(), pglogrepl.LSN(0x1100); got != want {
		t.Fatalf("SyncedLSN = %s after the commit was archived, want its end LSN %s", got, want)
	}
}
