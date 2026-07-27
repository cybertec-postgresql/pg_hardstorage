package walsink_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
)

// A WAL skip that lands EXACTLY on a segment boundary used to commit
// silently: right after a hand-off there is no current segment, so
// the per-segment guards (segNum mismatch, offset mismatch) never
// ran, the sink adopted the later segment verbatim, and SyncedLSN
// advanced past an unrecorded hole — PG then recycles the missing
// WAL and the gap becomes permanent and unhealable. The stream-level
// expectedNext check must refuse ANY record that does not start
// exactly where the previous one ended.
func TestSink_GapOnSegmentBoundary_Refused(t *testing.T) {
	s, sp := newTestSink(t)

	// Fill segment 0 exactly.
	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(0),
		Data:     fillBytes(0x21, walsink.SegmentSize),
	}); err != nil {
		t.Fatalf("segment 0: %v", err)
	}

	// Next record skips segment 1 entirely and starts at segment 2's
	// first byte — boundary-aligned, so both legacy guards are blind.
	err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(2 * walsink.SegmentSize),
		Data:     fillBytes(0x22, walsink.SegmentSize),
	})
	if err == nil {
		t.Fatal("boundary-aligned WAL skip was accepted — segment 2 would commit past an unrecorded hole")
	}
	if !strings.Contains(err.Error(), "gap detected") {
		t.Errorf("error = %v, want a walsink gap refusal", err)
	}

	// The skipped-past segment must never have been committed.
	if err := s.Flush(context.Background()); err != nil {
		t.Logf("flush after refused record (best-effort): %v", err)
	}
	if _, err := sp.Get(context.Background(),
		"wal/db1/00000001/000000010000000000000002.json"); err == nil {
		t.Error("segment 2 manifest exists — a segment past the hole was committed")
	}
	if got := uint64(s.SyncedLSN()); got > walsink.SegmentSize {
		t.Errorf("SyncedLSN = %x advanced past the hole (max allowed %x)", got, walsink.SegmentSize)
	}
}

// Mid-segment gaps must also refuse via the same stream-level check
// (previously caught by the offset guard — pinned here so the new
// check demonstrably subsumes it).
func TestSink_GapMidSegment_Refused(t *testing.T) {
	s, _ := newTestSink(t)

	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(0),
		Data:     fillBytes(0x31, 4096),
	}); err != nil {
		t.Fatalf("first record: %v", err)
	}
	err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(8192), // 4 KiB hole inside segment 0
		Data:     fillBytes(0x32, 4096),
	})
	if err == nil || !strings.Contains(err.Error(), "gap detected") {
		t.Fatalf("mid-segment skip: err = %v, want gap refusal", err)
	}
}
