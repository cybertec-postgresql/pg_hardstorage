package webhook_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical/commitlsn"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical/sinks/webhook"
)

// A POSTed batch that ends mid-transaction must not confirm the
// synthetic WALStart+len(Data) of its last row: that overshoots the
// transaction's own (not yet received) commit, and PostgreSQL skips
// the transaction after a restart. Only commit end LSNs are confirmed.
func TestSink_ConfirmsOnlyCommitEndLSNs(t *testing.T) {
	rec := newRecorder()
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	s, err := webhook.New(webhook.Options{URL: srv.URL, BatchSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	_ = s.OnRecord(ctx, mkRecord(0x1000, append([]byte{'B'}, make([]byte, 20)...)))
	_ = s.OnRecord(ctx, mkRecord(0x1010, append([]byte{'I'}, bytes.Repeat([]byte("x"), 8192)...)))
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := s.SyncedLSN(); got != 0 {
		t.Fatalf("SyncedLSN = %s after a mid-transaction batch; want 0 (no commit delivered)", got)
	}

	_ = s.OnRecord(ctx, mkRecord(0x1100, commitlsn.Message(0x1100)))
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := s.SyncedLSN(), pglogrepl.LSN(0x1100); got != want {
		t.Fatalf("SyncedLSN = %s, want the commit end LSN %s", got, want)
	}
}
