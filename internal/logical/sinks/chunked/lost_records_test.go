package chunked_test

// commitManifest treats ErrAlreadyExists as success:
//
//	// Idempotent: a previous flush at this start_lsn
//	// already committed this batch. Chunks deduplicated
//	// naturally; the existing manifest is correct.
//	return nil
//
// "the existing manifest is correct" holds only when the previous
// attempt wrote the SAME batch. CommitExclusive makes a single attempt
// and does not retry, so ErrAlreadyExists means some EARLIER flush put
// that key there — and between the two, the stream kept delivering.
//
// flushLocked keeps the buffer and startLSN on error (correct: retry
// later), so the retry's batch is LARGER: same start_lsn, more records,
// a higher end_lsn. It hits ErrAlreadyExists against the smaller
// manifest, is told "correct", and then:
//
//	s.syncedLSN.Store(uint64(s.endLSN))   // past records never committed
//	s.buf.Reset()                          // and the bytes are dropped
//
// The extra records exist in the CAS as chunks referenced by no
// manifest — so `repo gc` reaps them — while syncedLSN, which drives
// the slot's confirmed_flush_lsn, has advanced past them and PG
// releases that WAL. They are gone from the archive, silently, and the
// sink reported success.
//
// The trigger is ordinary for object storage: a PUT that lands
// server-side and then fails on the response (timeout, reset, 5xx after
// commit).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical/sinks/chunked"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/logicalreceiver"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// lostAckSP lets the first manifest commit LAND and then reports it as
// failed — the classic "succeeded server-side, failed on the wire".
type lostAckSP struct {
	storage.StoragePlugin
	failedOnce bool
}

func (s *lostAckSP) isManifest(key string) bool {
	return len(key) > 8 && key[:8] == "logical/" && !bytes.Contains([]byte(key), []byte(".tmp."))
}

func (s *lostAckSP) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	res, err := s.StoragePlugin.Put(ctx, key, r, opts)
	if err == nil && !s.failedOnce && s.isManifest(key) {
		s.failedOnce = true
		return res, errors.New("connection reset after the object was written")
	}
	return res, err
}

func (s *lostAckSP) RenameIfNotExists(ctx context.Context, src, dst string) error {
	err := s.StoragePlugin.RenameIfNotExists(ctx, src, dst)
	if err == nil && !s.failedOnce && s.isManifest(dst) {
		s.failedOnce = true
		return errors.New("connection reset after the rename committed")
	}
	return err
}

func TestSink_LargerRetryBatchMustNotBeSilentlyDropped(t *testing.T) {
	ctx := context.Background()
	inner := openFS(t, "file://"+t.TempDir())
	defer inner.Close()
	sp := &lostAckSP{StoragePlugin: inner}
	cas := casdefault.New(sp)

	s, err := chunked.New(cas, sp, chunked.Options{
		Deployment: "db1", StreamName: "events", Slot: "slot1",
		BatchBytes: 1 << 20, // large: only explicit Flush commits
	})
	if err != nil {
		t.Fatal(err)
	}

	// First batch: one record. Its commit LANDS but reports failure.
	first := logicalreceiver.Record{WALStart: 0x1000, Data: bytes.Repeat([]byte("a"), 64)}
	if err := s.OnRecord(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err == nil {
		t.Fatal("fixture did not inject the lost-ack failure")
	}

	// The stream keeps delivering while the sink holds the batch.
	second := logicalreceiver.Record{WALStart: 0x1040, Data: bytes.Repeat([]byte("b"), 64)}
	third := logicalreceiver.Record{WALStart: 0x1080, Data: bytes.Repeat([]byte("c"), 64)}
	for _, r := range []logicalreceiver.Record{second, third} {
		if err := s.OnRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	// The retry: same start_lsn, three records now.
	flushErr := s.Flush(ctx)

	// Whatever the sink decides, it must not claim to have archived
	// records the committed manifest does not cover.
	committed := readCommittedManifest(t, inner, "logical/db1/events/0_1000.json")
	synced := s.SyncedLSN()

	endLSN, perr := pglogrepl.ParseLSN(committed.EndLSN)
	if perr != nil {
		t.Fatalf("committed manifest has an unparseable end_lsn %q: %v", committed.EndLSN, perr)
	}

	if flushErr == nil && uint64(synced) > uint64(endLSN) {
		t.Fatalf("sink reported success and advanced SyncedLSN to %s, but the committed "+
			"manifest only covers up to %s (%d records) — the records in between were "+
			"chunked into the CAS, are referenced by no manifest (so gc will reap them), "+
			"and the slot's confirmed_flush_lsn has moved past them so PG will drop that "+
			"WAL. They are lost from the logical archive, silently.",
			synced, endLSN, committed.Records)
	}
}

func readCommittedManifest(t *testing.T, sp storage.StoragePlugin, key string) chunked.SegmentManifest {
	t.Helper()
	rc, err := sp.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	var m chunked.SegmentManifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	return m
}

// After the refusal the sink must have kept everything, so the stream
// can resume once the operator resolves the short manifest. A refusal
// that still dropped the buffer or moved syncedLSN would be the same
// data loss with an error message attached.
func TestSink_RefusalRetainsBufferAndSyncedLSN(t *testing.T) {
	ctx := context.Background()
	inner := openFS(t, "file://"+t.TempDir())
	defer inner.Close()
	sp := &lostAckSP{StoragePlugin: inner}
	cas := casdefault.New(sp)

	s, err := chunked.New(cas, sp, chunked.Options{
		Deployment: "db1", StreamName: "events", Slot: "slot1", BatchBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.OnRecord(ctx, logicalreceiver.Record{
		WALStart: 0x1000, Data: bytes.Repeat([]byte("a"), 64)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err == nil {
		t.Fatal("fixture did not inject the lost-ack failure")
	}
	before := s.SyncedLSN()

	if err := s.OnRecord(ctx, logicalreceiver.Record{
		WALStart: 0x1040, Data: bytes.Repeat([]byte("b"), 64)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err == nil {
		t.Fatal("the diverged collision must be refused, not reported as success")
	}
	if got := s.SyncedLSN(); got != before {
		t.Errorf("SyncedLSN moved from %s to %s across a refused flush — the slot would "+
			"advance past records that were never committed", before, got)
	}
	// The buffer must still hold the batch: a further flush attempt
	// still sees the same divergence rather than an empty batch.
	if err := s.Flush(ctx); err == nil {
		t.Error("a repeated flush after the refusal succeeded, which means the buffered " +
			"records were dropped rather than retained")
	}
}

// The genuinely-idempotent case must stay silent: an existing manifest
// covering the SAME batch is what a retried commit legitimately finds.
func TestSink_IdenticalRetryIsStillIdempotent(t *testing.T) {
	ctx := context.Background()
	inner := openFS(t, "file://"+t.TempDir())
	defer inner.Close()
	sp := &lostAckSP{StoragePlugin: inner}
	cas := casdefault.New(sp)

	mk := func() *chunked.Sink {
		s, err := chunked.New(cas, sp, chunked.Options{
			Deployment: "db1", StreamName: "events", Slot: "slot1", BatchBytes: 1 << 20,
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	rec := logicalreceiver.Record{WALStart: 0x1000, Data: bytes.Repeat([]byte("a"), 64)}

	s1 := mk()
	if err := s1.OnRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := s1.Flush(ctx); err == nil {
		t.Fatal("fixture did not inject the lost-ack failure")
	}
	// A fresh sink replays exactly the same batch (what PG does after a
	// restart from the unadvanced confirmed_flush_lsn).
	s2 := mk()
	if err := s2.OnRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := s2.Flush(ctx); err != nil {
		t.Fatalf("an identical replayed batch must be idempotent, not refused: %v", err)
	}
	if s2.SyncedLSN() == 0 {
		t.Error("SyncedLSN did not advance for a batch that is committed on disk")
	}
}

// An existing manifest we cannot read is not evidence that our batch is
// already archived. Treating the collision as idempotent there would
// drop the buffered records on the strength of an object we never
// decoded — the same loss, reached by a different door.
func TestSink_UnreadableExistingManifestIsRefused(t *testing.T) {
	ctx := context.Background()
	sp := openFS(t, "file://"+t.TempDir())
	defer sp.Close()
	cas := casdefault.New(sp)

	// Plant a corrupt object exactly where the batch will commit.
	key := "logical/db1/events/0_1000.json"
	body := []byte(`{"schema":"pg_hardstorage.logical_segment.v1","end_lsn":`) // truncated
	if _, err := sp.Put(ctx, key, bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}

	s, err := chunked.New(cas, sp, chunked.Options{
		Deployment: "db1", StreamName: "events", Slot: "slot1", BatchBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.OnRecord(ctx, logicalreceiver.Record{
		WALStart: 0x1000, Data: bytes.Repeat([]byte("a"), 64)}); err != nil {
		t.Fatal(err)
	}

	err = s.Flush(ctx)
	if err == nil {
		t.Fatal("a collision against an UNREADABLE manifest was reported as success — the " +
			"buffered records were dropped and syncedLSN advanced on the strength of an " +
			"object that was never decoded")
	}
	if s.SyncedLSN() != 0 {
		t.Errorf("SyncedLSN advanced to %s despite the refusal", s.SyncedLSN())
	}
}

// A manifest whose end_lsn will not parse is the same situation: we
// cannot establish coverage, so we cannot claim idempotency.
func TestSink_ExistingManifestWithBadEndLSNIsRefused(t *testing.T) {
	ctx := context.Background()
	sp := openFS(t, "file://"+t.TempDir())
	defer sp.Close()
	cas := casdefault.New(sp)

	key := "logical/db1/events/0_1000.json"
	body := []byte(`{"schema":"pg_hardstorage.logical_segment.v1","start_lsn":"0/1000",` +
		`"end_lsn":"not-an-lsn","records":1}`)
	if _, err := sp.Put(ctx, key, bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}

	s, err := chunked.New(cas, sp, chunked.Options{
		Deployment: "db1", StreamName: "events", Slot: "slot1", BatchBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.OnRecord(ctx, logicalreceiver.Record{
		WALStart: 0x1000, Data: bytes.Repeat([]byte("a"), 64)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err == nil {
		t.Fatal("a collision against a manifest with an unparseable end_lsn was reported " +
			"as success; coverage could not be established, so idempotency cannot be claimed")
	}
}
