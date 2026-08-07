package walsink_test

// gap_reconnect_test.go — the contiguity guard has to cover the FIRST
// record, not just the ones after it.
//
// The stream-level check compares each record against the end of the
// previous one, and expectedNext is zero until a record arrives. That
// is fine for one continuous stream. `wal stream` is not one continuous
// stream: it builds a NEW Sink on every reconnect attempt, so after
// each reconnect the opening record was accepted at whatever LSN it
// carried, and every later record was then measured against THAT. A
// stream that resumed past a hole looked perfectly contiguous forever
// after.
//
// This is the net beneath the resume fixes, and it was the wrong shape.
// Those fixes make the requested resume position correct; this makes
// the sink notice when what arrives does not match what was requested.
// A net that only holds when the thing above it is already right is not
// a net.
//
// Only the FORWARD direction is a fault. A record starting after the
// requested position means the bytes in between were never sent and are
// not coming. A record starting at or before it is normal — a walsender
// may open on a page or segment boundary at or below what was asked for
// — and refusing that would turn a healthy stream into a crash loop,
// which is a worse way to lose WAL than the bug being fixed.

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// newResumingSink builds a Sink that was told where the stream is
// expected to open — the shape `wal stream` creates on every reconnect.
func newResumingSink(t *testing.T, expectFirst pglogrepl.LSN) (*walsink.Sink, storage.StoragePlugin) {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	s, err := walsink.New(repo.NewCAS(sp), sp, walsink.Options{
		Deployment:       "db1",
		Timeline:         1,
		SystemIdentifier: "7388123456789",
		ExpectedFirstLSN: expectFirst,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s, sp
}

// TestSink_OpeningRecordPastTheResumePoint_Refused is the regression
// test.
//
// The sink is told the stream resumes at segment 1, and PG opens at
// segment 3 instead — segment-boundary aligned, so every per-segment
// guard is blind, and it is the first record so expectedNext is zero.
// Two whole segments are missing and are not coming.
func TestSink_OpeningRecordPastTheResumePoint_Refused(t *testing.T) {
	const resumeAt = 1 * walsink.SegmentSize
	s, sp := newResumingSink(t, pglogrepl.LSN(resumeAt))

	err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(3 * walsink.SegmentSize),
		Data:     fillBytes(0x31, walsink.SegmentSize),
	})
	if err == nil {
		t.Fatal("the opening record began two segments past the requested resume point and " +
			"was accepted.\n\n" +
			"`wal stream` builds a fresh Sink on every reconnect, so this is the state after " +
			"EVERY reconnect: the first record sets the baseline and everything afterwards is " +
			"measured against it. A stream that resumed past a hole would look contiguous for " +
			"the rest of its life, and PG recycles the missing WAL.")
	}
	if !strings.Contains(err.Error(), "gap detected") {
		t.Errorf("error = %v, want a walsink gap refusal", err)
	}
	// The message has to carry the arithmetic; "gap detected" alone
	// leaves the operator to work out how much is missing.
	if !strings.Contains(err.Error(), "skipped") {
		t.Errorf("the refusal does not say how much was skipped: %v", err)
	}

	if ferr := s.Flush(context.Background()); ferr != nil {
		t.Logf("flush after refused record (best-effort): %v", ferr)
	}
	if _, gerr := sp.Get(context.Background(),
		"wal/db1/00000001/000000010000000000000003.json"); gerr == nil {
		t.Error("segment 3 committed despite the refusal — a segment past the hole reached the repo")
	}
	if got := s.SyncedLSN(); got != 0 {
		t.Errorf("SyncedLSN = %s advanced despite the refusal", got)
	}
}

// TestSink_OpeningRecordAtTheResumePoint_Accepted: the ordinary
// reconnect. This is what every healthy resume looks like, and it must
// not be disturbed.
func TestSink_OpeningRecordAtTheResumePoint_Accepted(t *testing.T) {
	const resumeAt = 2 * walsink.SegmentSize
	s, _ := newResumingSink(t, pglogrepl.LSN(resumeAt))

	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(resumeAt),
		Data:     fillBytes(0x32, walsink.SegmentSize),
	}); err != nil {
		t.Fatalf("a stream opening exactly at the requested resume point was refused: %v", err)
	}
	flush(t, s)
}

// TestSink_OpeningRecordBeforeTheResumePoint_Accepted pins the
// direction of the check, and it is the case that decides whether this
// guard is safe to ship.
//
// A walsender may begin at a page or segment boundary at or below what
// was requested. Those extra bytes are free and harmless. Refusing them
// would make the streamer crash-loop against a perfectly healthy
// primary — trading a rare silent gap for a guaranteed outage, which is
// the worse of the two failures.
func TestSink_OpeningRecordBeforeTheResumePoint_Accepted(t *testing.T) {
	const resumeAt = 3 * walsink.SegmentSize
	s, _ := newResumingSink(t, pglogrepl.LSN(resumeAt))

	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(2 * walsink.SegmentSize),
		Data:     fillBytes(0x33, walsink.SegmentSize),
	}); err != nil {
		t.Fatalf("a stream opening BEFORE the requested resume point was refused: %v\n\n"+
			"Receiving more WAL than asked for is not a gap. Refusing it turns a healthy "+
			"stream into a crash loop, which loses more WAL than the hole this guard exists "+
			"to catch.", err)
	}
	flush(t, s)
}

// TestSink_StrictContiguityStillAppliesAfterTheOpeningRecord: the new
// first-record check must not have replaced the existing per-record
// one. The opening record is checked loosely (forward only); everything
// after it stays exact.
func TestSink_StrictContiguityStillAppliesAfterTheOpeningRecord(t *testing.T) {
	const resumeAt = 1 * walsink.SegmentSize
	s, _ := newResumingSink(t, pglogrepl.LSN(resumeAt))

	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(resumeAt),
		Data:     fillBytes(0x34, walsink.SegmentSize),
	}); err != nil {
		t.Fatalf("opening record: %v", err)
	}
	// Now skip a segment mid-stream. Backwards would be caught too, but
	// forwards is the hole.
	err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(3 * walsink.SegmentSize),
		Data:     fillBytes(0x35, walsink.SegmentSize),
	})
	if err == nil {
		t.Fatal("a mid-stream segment skip was accepted; the first-record check must ADD to " +
			"the strict per-record check, not replace it")
	}
	if !strings.Contains(err.Error(), "gap detected") {
		t.Errorf("error = %v, want a gap refusal", err)
	}
}

// TestSink_NoExpectedFirstLSN_BehavesAsBefore: zero disables the check.
// The --no-slot path can resolve to LSN 0, and unit callers construct
// sinks without it; neither should start failing.
func TestSink_NoExpectedFirstLSN_BehavesAsBefore(t *testing.T) {
	s, _ := newResumingSink(t, 0)
	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(5 * walsink.SegmentSize),
		Data:     fillBytes(0x36, walsink.SegmentSize),
	}); err != nil {
		t.Fatalf("with no expected first LSN the sink must accept any opening record: %v", err)
	}
	flush(t, s)
}
