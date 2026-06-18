package walsink_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// newTestSink returns a Sink + the underlying StoragePlugin so tests
// can inspect what was actually written.
func newTestSink(t *testing.T) (*walsink.Sink, storage.StoragePlugin) {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	cas := repo.NewCAS(sp)
	s, err := walsink.New(cas, sp, walsink.Options{
		Deployment:       "db1",
		Timeline:         1,
		SystemIdentifier: "7388123456789",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The Sink runs a background processor goroutine; Close drains and
	// stops it. Idempotent, so a test that already called Close/Flush
	// is unaffected.
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s, sp
}

// flush drains the async Sink: it blocks until every segment handed
// off so far is committed, so SyncedLSN and the on-disk manifests
// reflect the records fed before it. Fatals on a processing error.
func flush(t *testing.T, s *walsink.Sink) {
	t.Helper()
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// fillBytes returns a deterministic 16 MiB-or-arbitrary-size byte slice.
// Each segment uses a different seed value so dedup analysis in tests
// works as expected.
func fillBytes(seed byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = seed + byte(i)
	}
	return out
}

func TestSink_SingleSegment_RoundTrip(t *testing.T) {
	s, sp := newTestSink(t)

	body := fillBytes(0x10, walsink.SegmentSize)
	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(0),
		Data:     body,
	}); err != nil {
		t.Fatalf("OnRecord: %v", err)
	}
	flush(t, s)

	if got := uint64(s.SyncedLSN()); got != walsink.SegmentSize {
		t.Errorf("SyncedLSN = %x, want %x", got, walsink.SegmentSize)
	}

	// Manifest at canonical path.
	want := "wal/db1/00000001/000000010000000000000000.json"
	rc, err := sp.Get(context.Background(), want)
	if err != nil {
		t.Fatalf("manifest missing at %q: %v", want, err)
	}
	defer rc.Close()
	var m walsink.SegmentManifest
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if m.Schema != walsink.Schema {
		t.Errorf("Schema = %q, want %q", m.Schema, walsink.Schema)
	}
	if m.Timeline != 1 {
		t.Errorf("Timeline = %d, want 1", m.Timeline)
	}
	if m.SegmentNumber != 0 {
		t.Errorf("SegmentNumber = %d, want 0", m.SegmentNumber)
	}
	if m.SegmentSize != walsink.SegmentSize {
		t.Errorf("SegmentSize = %d, want %d", m.SegmentSize, walsink.SegmentSize)
	}
	if m.SystemIdentifier != "7388123456789" {
		t.Errorf("SystemIdentifier = %q", m.SystemIdentifier)
	}
	if m.SegmentName != "000000010000000000000000" {
		t.Errorf("SegmentName = %q", m.SegmentName)
	}
	if len(m.Chunks) == 0 {
		t.Error("manifest has no chunks for a 16 MiB segment")
	}
	// Chunk lengths must sum to SegmentSize and offsets must be
	// strictly increasing and contiguous.
	var sum int64
	for i, c := range m.Chunks {
		if c.Offset != sum {
			t.Errorf("chunk %d: offset = %d, want %d (gap or overlap)", i, c.Offset, sum)
		}
		sum += c.Len
	}
	if sum != int64(walsink.SegmentSize) {
		t.Errorf("chunks total = %d bytes, want %d", sum, walsink.SegmentSize)
	}
}

func TestSink_TwoSegments_LSNAdvancesIncrementally(t *testing.T) {
	s, sp := newTestSink(t)

	// Segment 0.
	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: 0, Data: fillBytes(0xAA, walsink.SegmentSize),
	}); err != nil {
		t.Fatal(err)
	}
	flush(t, s)
	if got := uint64(s.SyncedLSN()); got != walsink.SegmentSize {
		t.Errorf("after seg 0: SyncedLSN = %x, want %x", got, walsink.SegmentSize)
	}

	// Segment 1 — different content so dedup doesn't elide chunks.
	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(walsink.SegmentSize),
		Data:     fillBytes(0xBB, walsink.SegmentSize),
	}); err != nil {
		t.Fatal(err)
	}
	flush(t, s)
	if got := uint64(s.SyncedLSN()); got != 2*walsink.SegmentSize {
		t.Errorf("after seg 1: SyncedLSN = %x, want %x", got, 2*walsink.SegmentSize)
	}

	// Both manifests on disk.
	for _, name := range []string{
		"wal/db1/00000001/000000010000000000000000.json",
		"wal/db1/00000001/000000010000000000000001.json",
	} {
		if _, err := sp.Stat(context.Background(), name); err != nil {
			t.Errorf("manifest missing at %q: %v", name, err)
		}
	}
}

func TestSink_PartialSegment_DoesNotAdvanceSyncedLSN(t *testing.T) {
	s, sp := newTestSink(t)

	half := fillBytes(0xCC, walsink.SegmentSize/2)
	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: 0, Data: half,
	}); err != nil {
		t.Fatal(err)
	}
	if got := s.SyncedLSN(); got != 0 {
		t.Errorf("partial segment must not advance SyncedLSN; got %v", got)
	}
	// No manifest written for the half-filled segment.
	if _, err := sp.Stat(context.Background(),
		"wal/db1/00000001/000000010000000000000000.json"); err == nil {
		t.Error("partial segment must not commit a manifest")
	}
}

func TestSink_RecordSpansSegmentBoundary(t *testing.T) {
	s, _ := newTestSink(t)

	// One record carrying 1.5 segments — the first segment fills,
	// commits, then half of the next segment is buffered.
	n := walsink.SegmentSize + walsink.SegmentSize/2
	body := fillBytes(0x42, n)
	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: 0, Data: body,
	}); err != nil {
		t.Fatal(err)
	}
	flush(t, s)
	// Only segment 0 committed; segment 1 is half-filled.
	if got := uint64(s.SyncedLSN()); got != walsink.SegmentSize {
		t.Errorf("SyncedLSN = %x, want %x (only first segment committed)",
			got, walsink.SegmentSize)
	}

	// Completing the second segment with another record commits it too.
	rest := fillBytes(0x42, walsink.SegmentSize/2)
	// the WALStart of the continuation is 1.5 * SegmentSize.
	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(walsink.SegmentSize + walsink.SegmentSize/2),
		Data:     rest,
	}); err != nil {
		t.Fatal(err)
	}
	flush(t, s)
	if got := uint64(s.SyncedLSN()); got != 2*walsink.SegmentSize {
		t.Errorf("after completion: SyncedLSN = %x, want %x", got, 2*walsink.SegmentSize)
	}
}

func TestSink_ManySmallRecords_FillSegment(t *testing.T) {
	s, _ := newTestSink(t)

	// Drip-feed 4 KiB at a time. The pipe of 4096 of them fills one
	// segment exactly. This mirrors what PG's wire format actually
	// does: many small CopyData messages.
	const chunk = 4096
	for off := 0; off < walsink.SegmentSize; off += chunk {
		if err := s.OnRecord(context.Background(), replication.XLogRecord{
			WALStart: pglogrepl.LSN(off),
			Data:     fillBytes(0x77, chunk),
		}); err != nil {
			t.Fatalf("at offset %d: %v", off, err)
		}
	}
	flush(t, s)
	if got := uint64(s.SyncedLSN()); got != walsink.SegmentSize {
		t.Errorf("after drip-feed: SyncedLSN = %x, want %x", got, walsink.SegmentSize)
	}
}

func TestSink_OutOfOrderOffset_Rejected(t *testing.T) {
	s, _ := newTestSink(t)

	// First write puts 4 KiB at offset 0.
	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: 0, Data: fillBytes(0x01, 4096),
	}); err != nil {
		t.Fatal(err)
	}

	// Second write claims offset 8 KiB — but Sink expects 4 KiB.
	err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: 8192, Data: fillBytes(0x02, 4096),
	})
	if err == nil {
		t.Fatal("expected error on non-sequential WAL")
	}
	if !strings.Contains(err.Error(), "out-of-order") {
		t.Errorf("error should mention out-of-order; got %v", err)
	}
}

func TestSink_GapBetweenSegments_Rejected(t *testing.T) {
	s, _ := newTestSink(t)

	// Half-fill segment 0.
	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: 0, Data: fillBytes(0x01, walsink.SegmentSize/2),
	}); err != nil {
		t.Fatal(err)
	}

	// Jump to segment 1 — segment 0 is incomplete.
	err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(walsink.SegmentSize),
		Data:     fillBytes(0x02, 4096),
	})
	if err == nil {
		t.Fatal("expected gap error")
	}
	if !strings.Contains(err.Error(), "gap detected") {
		t.Errorf("error should mention gap; got %v", err)
	}
}

func TestSink_IdempotentReCommit(t *testing.T) {
	// Two Sinks against the same repo with the same input. The
	// second's commit hits an existing manifest; we expect success
	// (idempotent), not an error.
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	cas := repo.NewCAS(sp)

	mk := func() *walsink.Sink {
		s, err := walsink.New(cas, sp, walsink.Options{
			Deployment:       "db1",
			Timeline:         1,
			SystemIdentifier: "7388123456789",
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	body := fillBytes(0x99, walsink.SegmentSize)
	rec := replication.XLogRecord{WALStart: 0, Data: body}

	// Close drains the async pipeline — it is what actually commits
	// the segment, and surfaces any processing error.
	s1 := mk()
	if err := s1.OnRecord(context.Background(), rec); err != nil {
		t.Fatalf("first OnRecord: %v", err)
	}
	if err := s1.Close(context.Background()); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	// Second Sink: same input against the same repo — its commit hits
	// the existing manifest and must succeed idempotently.
	s2 := mk()
	if err := s2.OnRecord(context.Background(), rec); err != nil {
		t.Fatalf("second OnRecord: %v", err)
	}
	if err := s2.Close(context.Background()); err != nil {
		t.Errorf("idempotent re-commit returned error: %v", err)
	}
}

// A ctx cancelled BEFORE finalizeSegment commits the manifest must
// abort the commit — neither the manifest nor SyncedLSN should
// advance. The implicit ctx propagation through commitManifest's
// Put call already gives this behaviour, but bug-review pass 9
// added an explicit early ctx.Err() check between the chunk loop
// and the manifest build (defense in depth + cheaper failure path
// for already-cancelled cases).
func TestSink_CtxCancelled_DoesNotCommitManifest(t *testing.T) {
	s, sp := newTestSink(t)

	// Pre-cancelled ctx — every ctx.Err() check fires immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	body := fillBytes(0x55, walsink.SegmentSize)
	err := s.OnRecord(ctx, replication.XLogRecord{
		WALStart: 0,
		Data:     body,
	})
	if err == nil {
		t.Fatal("expected ctx.Canceled error; got nil")
	}
	// SyncedLSN must NOT advance — the segment was never committed.
	if got := s.SyncedLSN(); got != 0 {
		t.Errorf("SyncedLSN advanced to %v despite cancellation", got)
	}
	// The manifest must NOT exist on disk. (A canceled commit might
	// have written the .tmp; that's fine for cleanup. The canonical
	// key MUST NOT be there.)
	want := "wal/db1/00000001/000000010000000000000000.json"
	if _, err := sp.Stat(context.Background(), want); err == nil {
		t.Errorf("manifest %q committed despite cancellation", want)
	}
}

// TestSink_FaultHook covers the resilience invariant the
// chunker→commit→LSN-advance pipeline relies on: a crash at
// ANY of the four named checkpoints, followed by a fresh
// Sink replaying the same WAL bytes against the same repo,
// must produce a final repo state byte-identical to a
// no-fault control run.  Pre-fault commits dedup naturally
// (chunks are content-addressed); post-fault commits land
// on either an empty key (no prior manifest) or the
// existing manifest path that hits ErrAlreadyExists →
// verifyExistingManifest, which we know is correct from
// the doppelgänger tests.
//
// One sub-test per checkpoint.  Each one:
//
//  1. Runs a control Sink to completion against repo A,
//     records the manifest path's body bytes.
//  2. Runs a faulted Sink against repo B with a hook that
//     errors at the named checkpoint AT segment 1's commit.
//     Asserts the Sink returns the sentinel error.
//  3. Runs a fresh recovery Sink against the SAME repo B
//     with the same WAL bytes.  Asserts no error, and the
//     final manifest body in B matches the body in A
//     byte-for-byte.
func TestSink_FaultHook(t *testing.T) {
	checkpoints := []string{
		walsink.HookAfterChunkUploaded,
		walsink.HookBeforeManifestCommit,
		walsink.HookAfterManifestRename,
		walsink.HookBeforeLSNAdvance,
	}
	for _, cp := range checkpoints {
		t.Run(cp, func(t *testing.T) {
			body := fillBytes(0x42, walsink.SegmentSize)

			// Control: clean run, fresh repo, no hook.
			ctrlBody := mustReadFinalManifest(t, t.TempDir(), body, nil, false)

			// Faulted run: hook errors at `cp`.
			sentinel := errors.New("synthetic-crash")
			faultRepoDir := t.TempDir()
			_ = mustReadFinalManifest(t, faultRepoDir, body, faultHookAt(cp, sentinel), true)

			// Recovery run: SAME repo dir, fresh Sink, no
			// hook.  PG's slot semantics mean the bytes
			// resume from segment-start; we drive the same
			// body through.
			resumedBody := mustReadFinalManifest(t, faultRepoDir, body, nil, false)
			if resumedBody == nil {
				t.Fatalf("resume produced no manifest at %s", cp)
			}
			if !bytes.Equal(stripTimestamp(ctrlBody), stripTimestamp(resumedBody)) {
				t.Errorf("resume manifest at %s differs from control (ignoring CreatedAt)", cp)
			}
		})
	}
}

// mustReadFinalManifest opens a fresh fs Sink at repoDir,
// drives body through OnRecord, and (when present) reads
// back the canonical segment manifest body bytes.  expectErr
// asserts whether OnRecord returned an error.  Caller-side
// repoDir lifecycle: persistent across calls so the recovery
// run can find the prior committed state.
func mustReadFinalManifest(t *testing.T, repoDir string, body []byte, hook walsink.FaultHook, expectErr bool) []byte {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: repoDir}}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	cas := repo.NewCAS(sp)
	s, err := walsink.New(cas, sp, walsink.Options{
		Deployment:       "fault-test",
		Timeline:         1,
		SystemIdentifier: "7388123456789",
		FaultHook:        hook,
	})
	if err != nil {
		t.Fatal(err)
	}
	rerr := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: pglogrepl.LSN(0),
		Data:     body,
	})
	// The async processor does the chunk → barrier → commit work; a
	// FaultHook error surfaces when Close drains the pipeline.
	if cerr := s.Close(context.Background()); rerr == nil {
		rerr = cerr
	}
	if expectErr && rerr == nil {
		t.Fatal("expected fault hook to surface as error; got nil")
	}
	if !expectErr && rerr != nil {
		t.Fatalf("unexpected sink error: %v", rerr)
	}
	prefix := "wal/fault-test/00000001/"
	for info, err := range sp.List(context.Background(), prefix) {
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(info.Key, ".json") && !strings.Contains(info.Key, ".tmp.") {
			rc, err := sp.Get(context.Background(), info.Key)
			if err != nil {
				t.Fatalf("get %s: %v", info.Key, err)
			}
			defer rc.Close()
			out, err := readAllBytes(rc)
			if err != nil {
				t.Fatalf("read %s: %v", info.Key, err)
			}
			return out
		}
	}
	return nil
}

func readAllBytes(rc interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return out, nil
			}
			return out, err
		}
	}
}

// stripTimestamp removes the manifest's CreatedAt field for
// the byte-equal compare — wall-clock differs between runs
// even on identical-content commits.  Cheap regex-style
// substitution; the manifest's stable format makes a
// substring match safe.
func stripTimestamp(body []byte) []byte {
	const marker = `"created_at":`
	idx := bytes.Index(body, []byte(marker))
	if idx < 0 {
		return body
	}
	end := bytes.IndexByte(body[idx:], ',')
	if end < 0 {
		end = bytes.IndexByte(body[idx:], '}')
		if end < 0 {
			return body
		}
	}
	out := make([]byte, 0, len(body))
	out = append(out, body[:idx]...)
	out = append(out, body[idx+end:]...)
	return out
}

// faultHookAt builds a FaultHook that fires the supplied
// error iff the checkpoint name matches; other checkpoints
// pass through.  Single-shot: after the first match it
// flips to a no-op so a resumed Sink isn't blocked.  The
// after_chunk_uploaded checkpoint fires from the processor's
// parallel chunk worker pool, so the latch is atomic.
func faultHookAt(name string, err error) walsink.FaultHook {
	var fired atomic.Bool
	return func(_ context.Context, cp string) error {
		if cp == name && fired.CompareAndSwap(false, true) {
			return err
		}
		return nil
	}
}

func TestSink_EmptyData_NoOp(t *testing.T) {
	s, _ := newTestSink(t)
	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: 0, Data: nil,
	}); err != nil {
		t.Errorf("empty data should be a no-op: %v", err)
	}
	if got := s.SyncedLSN(); got != 0 {
		t.Errorf("empty record must not advance SyncedLSN; got %v", got)
	}
}

func TestSink_RoundTrip_BytesReconstructFromCAS(t *testing.T) {
	// End-to-end correctness: a committed segment's chunks, fetched
	// from the CAS in manifest order, must reproduce the original
	// byte stream byte-for-byte. This is the contract the restore
	// path will rely on.
	s, sp := newTestSink(t)
	cas := repo.NewCAS(sp)

	body := fillBytes(0xEE, walsink.SegmentSize)
	if err := s.OnRecord(context.Background(), replication.XLogRecord{
		WALStart: 0, Data: body,
	}); err != nil {
		t.Fatal(err)
	}
	flush(t, s)

	rc, err := sp.Get(context.Background(),
		"wal/db1/00000001/000000010000000000000000.json")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var m walsink.SegmentManifest
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		t.Fatal(err)
	}

	// Reassemble.
	var got bytes.Buffer
	for _, c := range m.Chunks {
		bs, err := cas.GetChunkBytes(context.Background(), c.Hash)
		if err != nil {
			t.Fatalf("get chunk %s: %v", c.Hash, err)
		}
		if int64(len(bs)) != c.Len {
			t.Errorf("chunk %s len = %d, manifest says %d", c.Hash, len(bs), c.Len)
		}
		got.Write(bs)
	}
	if !bytes.Equal(got.Bytes(), body) {
		t.Errorf("reassembled bytes differ from original (lengths: got %d, want %d)",
			got.Len(), len(body))
	}
}

// TestSink_ManySegments_PipelineOrdering pushes more segments than the
// pipeline's buffer pool holds and more than one barrier batch spans,
// so it exercises buffer recycling, the adaptive batching, and the
// in-order manifest commit under sustained load. Every segment must
// commit exactly once, and SyncedLSN must end segment-aligned at the
// last one — proof the async processor neither drops nor reorders.
func TestSink_ManySegments_PipelineOrdering(t *testing.T) {
	s, sp := newTestSink(t)
	const segs = 20 // > pipelineDepth (16) and > batchCap (16)
	for i := 0; i < segs; i++ {
		body := fillBytes(byte(0x10+i), walsink.SegmentSize)
		if err := s.OnRecord(context.Background(), replication.XLogRecord{
			WALStart: pglogrepl.LSN(uint64(i) * walsink.SegmentSize),
			Data:     body,
		}); err != nil {
			t.Fatalf("segment %d OnRecord: %v", i, err)
		}
	}
	flush(t, s)

	if got := uint64(s.SyncedLSN()); got != uint64(segs)*walsink.SegmentSize {
		t.Errorf("SyncedLSN = %x, want %x", got, uint64(segs)*walsink.SegmentSize)
	}
	for i := 0; i < segs; i++ {
		name := fmt.Sprintf("wal/db1/00000001/%s.json",
			walsink.SegmentFileName(1, uint64(i), walsink.SegmentSize))
		if _, err := sp.Stat(context.Background(), name); err != nil {
			t.Errorf("segment %d manifest missing (%s): %v", i, name, err)
		}
	}
}

func TestNew_ValidatesOptions(t *testing.T) {
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	cas := repo.NewCAS(sp)

	cases := []struct {
		name string
		opts walsink.Options
		want string
	}{
		{"empty deployment", walsink.Options{SystemIdentifier: "x"}, "deployment"},
		{"empty system_id", walsink.Options{Deployment: "d"}, "SystemIdentifier"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := walsink.New(cas, sp, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v; want one mentioning %q", err, tc.want)
			}
		})
	}

	if _, err := walsink.New(nil, sp, walsink.Options{Deployment: "d", SystemIdentifier: "x"}); err == nil {
		t.Error("nil CAS should error")
	}
	if _, err := walsink.New(cas, nil, walsink.Options{Deployment: "d", SystemIdentifier: "x"}); err == nil {
		t.Error("nil StoragePlugin should error")
	}
}

func TestSegmentFileName_Layout(t *testing.T) {
	cases := []struct {
		tli     uint32
		segNum  uint64
		want    string
		comment string
	}{
		{1, 0, "000000010000000000000000", "first segment"},
		{1, 3, "000000010000000000000003", "third segment of TLI 1"},
		{1, 256, "000000010000000100000000", "boundary into log_id 1"},
		{1, 257, "000000010000000100000001", "first seg of log_id 1"},
		{0xABCDEF, 0xCAFE_BABE, "00ABCDEF00CAFEBA000000BE", "non-trivial timeline"},
	}
	for _, c := range cases {
		got := walsink.SegmentFileName(c.tli, c.segNum, walsink.SegmentSize)
		if got != c.want {
			t.Errorf("%s: SegmentFileName(%#x, %d, walsink.SegmentSize) = %q, want %q",
				c.comment, c.tli, c.segNum, got, c.want)
		}
		if len(got) != 24 {
			t.Errorf("name %q has len %d, want 24", got, len(got))
		}
	}
}

func TestSegmentPath_Layout(t *testing.T) {
	cases := []struct {
		dep, name string
		tli       uint32
		want      string
	}{
		{"db1", "000000010000000000000003", 1, "wal/db1/00000001/000000010000000000000003.json"},
		// Caller may already include .json — strip and re-add.
		{"db1", "000000010000000000000003.json", 1, "wal/db1/00000001/000000010000000000000003.json"},
		{"prod-eu", "0000000A0000000B0000000C", 0xA, "wal/prod-eu/0000000A/0000000A0000000B0000000C.json"},
	}
	for _, c := range cases {
		got := walsink.SegmentPath(c.dep, c.tli, c.name)
		if got != c.want {
			t.Errorf("SegmentPath(%q,%#x,%q) = %q, want %q", c.dep, c.tli, c.name, got, c.want)
		}
	}
}

func TestParseSegmentManifest_RejectsBadSchema(t *testing.T) {
	raw := []byte(`{"schema":"some.future.schema.v9","deployment":"db1"}`)
	_, err := walsink.ParseSegmentManifest(raw)
	if err == nil {
		t.Fatal("expected schema-mismatch error")
	}
	if !strings.Contains(err.Error(), "schema") {
		t.Errorf("error should mention schema; got %v", err)
	}
}

func TestParseSegmentManifest_RoundTrip(t *testing.T) {
	original := &walsink.SegmentManifest{
		Schema:           walsink.Schema,
		Deployment:       "db1",
		SystemIdentifier: "7388123",
		Timeline:         2,
		SegmentNumber:    7,
		SegmentName:      walsink.SegmentFileName(2, 7, walsink.SegmentSize),
		StartLSN:         "0/7000000",
		EndLSN:           "0/8000000",
		SegmentSize:      walsink.SegmentSize,
	}
	body, err := original.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	got, err := walsink.ParseSegmentManifest(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.SegmentNumber != original.SegmentNumber || got.Timeline != original.Timeline ||
		got.Deployment != original.Deployment {
		t.Errorf("round-trip mismatch: %+v vs %+v", got, original)
	}
}

// Sanity-check that ctx cancellation aborts the finalize loop
// promptly. The Sink checks ctx between chunks; without that, a 16
// MiB segment with a fast in-memory backend would run uninterruptibly
// after cancellation.
func TestSink_ContextCancelled_PropagatesError(t *testing.T) {
	s, _ := newTestSink(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	body := fillBytes(0x55, walsink.SegmentSize)
	err := s.OnRecord(ctx, replication.XLogRecord{
		WALStart: 0, Data: body,
	})
	if err == nil {
		t.Fatal("expected ctx-cancelled error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled in error chain; got %v", err)
	}
	// SyncedLSN must NOT have advanced — the segment didn't commit.
	if got := s.SyncedLSN(); got != 0 {
		t.Errorf("SyncedLSN must stay at 0 on aborted finalize; got %v", got)
	}
}

// Coverage smoke: ensure the public surface compiles when imported
// from a typical caller. Touches every exported symbol once.
func TestPublicSurface_Smoke(t *testing.T) {
	_ = walsink.SegmentSize
	_ = walsink.Schema
	_ = walsink.SegmentFileName(1, 0, walsink.SegmentSize)
	_ = walsink.SegmentPath("d", 1, "x")
	if _, err := walsink.ParseSegmentManifest([]byte(fmt.Sprintf(`{"schema":%q}`, walsink.Schema))); err != nil {
		t.Errorf("smoke parse: %v", err)
	}
	m := &walsink.SegmentManifest{Schema: walsink.Schema}
	if _, err := m.MarshalToBytes(); err != nil {
		t.Errorf("smoke marshal: %v", err)
	}
}
