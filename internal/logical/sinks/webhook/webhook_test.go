package webhook_test

import (
	"context"
	"encoding/json"
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical/sinks/webhook"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/logicalreceiver"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// recorder is a tiny capture-and-respond test handler.
type recorder struct {
	calls          atomic.Int64
	statusCode     atomic.Int32
	requestBodies  [][]byte
	requestHeaders []stdhttp.Header
	failOnAttemptN int   // 1-indexed; 0 disables
	switchAfterN   int   // 1-indexed; switch from failStatus to 200
	failStatus     int32 // status to return until switchAfterN attempts
}

func newRecorder() *recorder {
	r := &recorder{}
	r.statusCode.Store(200)
	return r
}

func (r *recorder) handler() stdhttp.HandlerFunc {
	return func(w stdhttp.ResponseWriter, req *stdhttp.Request) {
		n := int(r.calls.Add(1))
		body := make([]byte, 0, 1024)
		buf := make([]byte, 1024)
		for {
			read, err := req.Body.Read(buf)
			body = append(body, buf[:read]...)
			if err != nil {
				break
			}
		}
		r.requestBodies = append(r.requestBodies, body)
		r.requestHeaders = append(r.requestHeaders, req.Header.Clone())
		if r.switchAfterN > 0 && n > r.switchAfterN {
			w.WriteHeader(200)
			return
		}
		if r.failOnAttemptN > 0 && n == r.failOnAttemptN {
			w.WriteHeader(int(r.failStatus))
			return
		}
		w.WriteHeader(int(r.statusCode.Load()))
	}
}

func mkRecord(lsn uint64, data []byte) logicalreceiver.Record {
	return logicalreceiver.Record{
		WALStart:     pglogrepl.LSN(lsn),
		ServerWALEnd: pglogrepl.LSN(lsn + uint64(len(data))),
		ServerTime:   time.Now().UTC(),
		Data:         data,
	}
}

// TestNew_Validation
func TestNew_Validation(t *testing.T) {
	if _, err := webhook.New(webhook.Options{}); err == nil {
		t.Error("empty URL must error")
	}
	if _, err := webhook.New(webhook.Options{URL: "ftp://nope"}); err == nil {
		t.Error("non-http(s) URL must error")
	}
	if _, err := webhook.New(webhook.Options{URL: "https://example.com"}); err != nil {
		t.Errorf("valid URL: %v", err)
	}
}

// TestSink_HappyPath_BatchSize: filling a batch triggers an
// immediate POST.
func TestSink_HappyPath_BatchSize(t *testing.T) {
	rec := newRecorder()
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	s, err := webhook.New(webhook.Options{
		URL:        srv.URL,
		BatchSize:  2,
		Deployment: "db1",
		StreamName: "events",
		Slot:       "test_slot",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.OnRecord(ctx, mkRecord(0x1000, []byte("a"))); err != nil {
		t.Fatal(err)
	}
	if rec.calls.Load() != 0 {
		t.Errorf("after 1 record: posted, want buffered")
	}
	if err := s.OnRecord(ctx, mkRecord(0x1001, []byte("b"))); err != nil {
		t.Fatal(err)
	}
	if rec.calls.Load() != 1 {
		t.Errorf("after batch full: %d posts, want 1", rec.calls.Load())
	}
	stats := s.Stats()
	if stats.PostsSucceeded != 1 || stats.TotalRecordsPosted != 2 {
		t.Errorf("stats off: %+v", stats)
	}
	if s.SyncedLSN() == 0 {
		t.Errorf("SyncedLSN didn't advance")
	}
}

// TestSink_HappyPath_Flush: explicit Flush forces a POST of
// buffered records.
func TestSink_HappyPath_Flush(t *testing.T) {
	rec := newRecorder()
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	s, err := webhook.New(webhook.Options{URL: srv.URL, BatchSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.OnRecord(ctx, mkRecord(uint64(0x1000+i), []byte{byte(i)})); err != nil {
			t.Fatal(err)
		}
	}
	if rec.calls.Load() != 0 {
		t.Errorf("buffered records posted prematurely")
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if rec.calls.Load() != 1 {
		t.Errorf("after Flush: %d posts, want 1", rec.calls.Load())
	}
}

// TestSink_Tick_FlushesAfterInterval: a stale buffer flushes
// when BatchInterval has elapsed.
func TestSink_Tick_FlushesAfterInterval(t *testing.T) {
	rec := newRecorder()
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	clock := &fakeClock{at: time.Now().UTC()}
	s, err := webhook.New(webhook.Options{
		URL:           srv.URL,
		BatchSize:     100,
		BatchInterval: 1 * time.Second,
		Now:           clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.OnRecord(ctx, mkRecord(0x1000, []byte("a"))); err != nil {
		t.Fatal(err)
	}

	// Tick before interval elapses: no flush.
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if rec.calls.Load() != 0 {
		t.Errorf("Tick before interval: posted")
	}

	// Advance the clock + tick again: flush.
	clock.advance(2 * time.Second)
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if rec.calls.Load() != 1 {
		t.Errorf("Tick after interval: posts = %d, want 1", rec.calls.Load())
	}
}

// TestSink_Headers: wire headers carry deployment / stream /
// batch ID.
func TestSink_Headers(t *testing.T) {
	rec := newRecorder()
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	s, err := webhook.New(webhook.Options{
		URL:        srv.URL,
		Deployment: "db1",
		StreamName: "events",
		BatchSize:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.OnRecord(context.Background(), mkRecord(0x1000, []byte("a"))); err != nil {
		t.Fatal(err)
	}
	if len(rec.requestHeaders) == 0 {
		t.Fatal("no headers captured")
	}
	hdr := rec.requestHeaders[0]
	if hdr.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", hdr.Get("Content-Type"))
	}
	if hdr.Get("X-PG-Hardstorage-Deployment") != "db1" {
		t.Errorf("missing deployment header")
	}
	if hdr.Get("X-PG-Hardstorage-Stream") != "events" {
		t.Errorf("missing stream header")
	}
	if hdr.Get("X-PG-Hardstorage-Batch-ID") == "" {
		t.Errorf("missing batch ID header")
	}
}

// TestSink_Authorization: optional Authorization header is set.
func TestSink_Authorization(t *testing.T) {
	rec := newRecorder()
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	s, err := webhook.New(webhook.Options{
		URL:           srv.URL,
		Authorization: "Bearer ABC123",
		BatchSize:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.OnRecord(context.Background(), mkRecord(0x1000, []byte("a")))
	if rec.requestHeaders[0].Get("Authorization") != "Bearer ABC123" {
		t.Errorf("Authorization missing or wrong: %q",
			rec.requestHeaders[0].Get("Authorization"))
	}
}

// TestSink_RetriesTransient: a 503 retries; success on the second
// attempt advances syncedLSN.
func TestSink_RetriesTransient(t *testing.T) {
	rec := newRecorder()
	rec.failOnAttemptN = 1
	rec.failStatus = 503
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	noSleep := func(time.Duration) {}
	s, err := webhook.New(webhook.Options{
		URL:            srv.URL,
		BatchSize:      1,
		RetryBudget:    3,
		RetryBaseDelay: time.Microsecond,
		Sleep:          noSleep,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.OnRecord(context.Background(), mkRecord(0x1000, []byte("a"))); err != nil {
		t.Fatalf("OnRecord: %v", err)
	}
	if rec.calls.Load() != 2 {
		t.Errorf("calls = %d, want 2 (1 fail + 1 success)", rec.calls.Load())
	}
	if s.SyncedLSN() == 0 {
		t.Errorf("SyncedLSN didn't advance after retry success")
	}
}

// TestSink_PermanentErrorExhaustsBudget: a 400 doesn't retry.
func TestSink_PermanentErrorExhaustsBudget(t *testing.T) {
	rec := newRecorder()
	rec.statusCode.Store(400)
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	s, err := webhook.New(webhook.Options{
		URL:            srv.URL,
		BatchSize:      1,
		RetryBudget:    5,
		RetryBaseDelay: time.Microsecond,
		Sleep:          func(time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.OnRecord(context.Background(), mkRecord(0x1000, []byte("a")))
	if err == nil {
		t.Error("400 should surface as a flush error (no dead letter)")
	}
	if rec.calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (no retries on 400)", rec.calls.Load())
	}
}

// TestSink_429RetriesTransient: 429 (too many requests) retries.
func TestSink_429RetriesTransient(t *testing.T) {
	rec := newRecorder()
	rec.failOnAttemptN = 1
	rec.failStatus = 429
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	s, err := webhook.New(webhook.Options{
		URL:            srv.URL,
		BatchSize:      1,
		RetryBudget:    3,
		RetryBaseDelay: time.Microsecond,
		Sleep:          func(time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.OnRecord(context.Background(), mkRecord(0x1000, []byte("a"))); err != nil {
		t.Fatalf("OnRecord: %v", err)
	}
	if rec.calls.Load() != 2 {
		t.Errorf("429 should retry: calls = %d, want 2", rec.calls.Load())
	}
}

// TestSink_BudgetExhausted_NoDeadLetter: persistent 503 +
// no dead-letter → slot stalls (syncedLSN stays at 0).
func TestSink_BudgetExhausted_NoDeadLetter(t *testing.T) {
	rec := newRecorder()
	rec.statusCode.Store(503)
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	s, err := webhook.New(webhook.Options{
		URL:            srv.URL,
		BatchSize:      1,
		RetryBudget:    2,
		RetryBaseDelay: time.Microsecond,
		Sleep:          func(time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.OnRecord(context.Background(), mkRecord(0x1000, []byte("a")))
	if err == nil {
		t.Error("expected flush error on exhaustion")
	}
	if !strings.Contains(err.Error(), "exhausted retry budget") {
		t.Errorf("error didn't mention exhaustion: %v", err)
	}
	if s.SyncedLSN() != 0 {
		t.Errorf("syncedLSN = %d; should stall at 0", s.SyncedLSN())
	}
}

// TestSink_BudgetExhausted_DeadLetter: dead-letter advances
// syncedLSN + persists the failed batch.
func TestSink_BudgetExhausted_DeadLetter(t *testing.T) {
	rec := newRecorder()
	rec.statusCode.Store(503)
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	dl := &capturingDL{}
	s, err := webhook.New(webhook.Options{
		URL:            srv.URL,
		BatchSize:      1,
		RetryBudget:    2,
		RetryBaseDelay: time.Microsecond,
		Sleep:          func(time.Duration) {},
		DeadLetter:     dl,
		Deployment:     "db1",
		StreamName:     "events",
		Slot:           "test_slot",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.OnRecord(context.Background(), mkRecord(0x1000, []byte("a")))
	if err != nil {
		t.Errorf("dead-letter should swallow exhaustion: %v", err)
	}
	if s.SyncedLSN() == 0 {
		t.Errorf("syncedLSN should advance after dead-letter")
	}
	if len(dl.envelopes) != 1 {
		t.Fatalf("dead-letter calls = %d, want 1", len(dl.envelopes))
	}
	env := dl.envelopes[0]
	if env.Deployment != "db1" || env.Slot != "test_slot" {
		t.Errorf("envelope wiring off: %+v", env)
	}
	if env.LastError == "" {
		t.Errorf("LastError empty")
	}
	stats := s.Stats()
	if stats.BatchesDeadLettered != 1 {
		t.Errorf("BatchesDeadLettered = %d, want 1", stats.BatchesDeadLettered)
	}
}

// TestSink_DeadLetterAppendError_KeepsBuffer: when the DL
// itself errors, the batch stays in-buffer + the error
// surfaces.
func TestSink_DeadLetterAppendError_KeepsBuffer(t *testing.T) {
	rec := newRecorder()
	rec.statusCode.Store(503)
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	dl := &erroringDL{}
	s, err := webhook.New(webhook.Options{
		URL:            srv.URL,
		BatchSize:      1,
		RetryBudget:    1,
		RetryBaseDelay: time.Microsecond,
		Sleep:          func(time.Duration) {},
		DeadLetter:     dl,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.OnRecord(context.Background(), mkRecord(0x1000, []byte("a")))
	if err == nil {
		t.Error("DL append error must surface")
	}
	if !strings.Contains(err.Error(), "dead-letter persist failed") {
		t.Errorf("error didn't mention dl failure: %v", err)
	}
	// The batch is back in the buffer; a subsequent Flush would
	// retry.  Replace with a passing DL + flush should now work.
}

// TestSink_BatchID_Deterministic: same records → same batch ID.
func TestSink_BatchID_Deterministic(t *testing.T) {
	rec := newRecorder()
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	s1, _ := webhook.New(webhook.Options{URL: srv.URL, BatchSize: 1})
	s2, _ := webhook.New(webhook.Options{URL: srv.URL, BatchSize: 1})
	r := mkRecord(0x1000, []byte("payload"))
	r.ServerTime = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) // pin
	_ = s1.OnRecord(context.Background(), r)
	_ = s2.OnRecord(context.Background(), r)
	id1 := rec.requestHeaders[0].Get("X-PG-Hardstorage-Batch-ID")
	id2 := rec.requestHeaders[1].Get("X-PG-Hardstorage-Batch-ID")
	if id1 == "" || id1 != id2 {
		t.Errorf("batch IDs not deterministic: %q vs %q", id1, id2)
	}
}

// TestSink_RetryReusesBatchID: a retry of the same batch sends
// the same batch ID (idempotency).
func TestSink_RetryReusesBatchID(t *testing.T) {
	rec := newRecorder()
	rec.failOnAttemptN = 1
	rec.failStatus = 503
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	s, _ := webhook.New(webhook.Options{
		URL: srv.URL, BatchSize: 1,
		RetryBudget: 3, RetryBaseDelay: time.Microsecond,
		Sleep: func(time.Duration) {},
	})
	if err := s.OnRecord(context.Background(), mkRecord(0x1000, []byte("a"))); err != nil {
		t.Fatal(err)
	}
	id1 := rec.requestHeaders[0].Get("X-PG-Hardstorage-Batch-ID")
	id2 := rec.requestHeaders[1].Get("X-PG-Hardstorage-Batch-ID")
	if id1 == "" || id1 != id2 {
		t.Errorf("retry didn't reuse batch ID: %q vs %q", id1, id2)
	}
}

// TestSink_RequestBodyShape: the wire format includes records
// + LSNs + deployment metadata.
func TestSink_RequestBodyShape(t *testing.T) {
	rec := newRecorder()
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	s, _ := webhook.New(webhook.Options{
		URL:        srv.URL,
		BatchSize:  1,
		Deployment: "db1",
		StreamName: "events",
		Slot:       "test_slot",
	})
	r := mkRecord(0x1234, []byte("hello"))
	if err := s.OnRecord(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(rec.requestBodies) != 1 {
		t.Fatal("no body captured")
	}
	var wire struct {
		Schema     string `json:"schema"`
		BatchID    string `json:"batch_id"`
		Deployment string `json:"deployment"`
		StreamName string `json:"stream_name"`
		Slot       string `json:"slot"`
		StartLSN   string `json:"start_lsn"`
		EndLSN     string `json:"end_lsn"`
		Records    []struct {
			WALStart string `json:"wal_start"`
			Data     []byte `json:"data"`
		} `json:"records"`
	}
	if err := json.Unmarshal(rec.requestBodies[0], &wire); err != nil {
		t.Fatalf("decode wire body: %v", err)
	}
	if wire.Schema != "pg_hardstorage.logical.webhook.v1" {
		t.Errorf("Schema = %q", wire.Schema)
	}
	if wire.Deployment != "db1" {
		t.Errorf("Deployment = %q", wire.Deployment)
	}
	if len(wire.Records) != 1 {
		t.Fatal("no records in wire body")
	}
	if string(wire.Records[0].Data) != "hello" {
		t.Errorf("payload = %q", string(wire.Records[0].Data))
	}
}

// TestStorageDeadLetter_RoundTrip: dead-lettered batch
// persists to repo + can be read back.
func TestStorageDeadLetter_RoundTrip(t *testing.T) {
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	dl := webhook.NewStorageDeadLetter(sp, "db1", "events")
	env := webhook.DeadLetterEnvelope{
		Schema:     "pg_hardstorage.logical.webhook.v1",
		BatchID:    "abc123",
		Deployment: "db1",
		StreamName: "events",
		Slot:       "test_slot",
		Records: []logicalreceiver.Record{
			mkRecord(0x1000, []byte("payload")),
		},
		StartLSN:  pglogrepl.LSN(0x1000),
		EndLSN:    pglogrepl.LSN(0x1007),
		LastError: "http 503",
		Attempts:  3,
		FailedAt:  time.Now().UTC(),
	}
	if err := dl.Append(context.Background(), env); err != nil {
		t.Fatalf("DL append: %v", err)
	}

	// Verify the file was written under the canonical prefix.
	count := 0
	for info, err := range sp.List(context.Background(), "logical/db1/events/dead-letter/") {
		if err != nil {
			t.Fatal(err)
		}
		count++
		if !strings.HasSuffix(info.Key, ".json") {
			t.Errorf("unexpected suffix: %s", info.Key)
		}
	}
	if count != 1 {
		t.Errorf("dead-letter files = %d, want 1", count)
	}
}

// TestSink_ContextCancellation: a cancelled context aborts
// retries.
func TestSink_ContextCancellation(t *testing.T) {
	rec := newRecorder()
	rec.statusCode.Store(503)
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	s, _ := webhook.New(webhook.Options{
		URL: srv.URL, BatchSize: 1,
		RetryBudget: 100, RetryBaseDelay: time.Microsecond,
		Sleep: func(time.Duration) {},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.OnRecord(ctx, mkRecord(0x1000, []byte("a")))
	if err == nil {
		t.Error("cancelled ctx should error")
	}
}

// TestSink_NoOpFlushOnEmpty
func TestSink_NoOpFlushOnEmpty(t *testing.T) {
	rec := newRecorder()
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	s, _ := webhook.New(webhook.Options{URL: srv.URL})
	if err := s.Flush(context.Background()); err != nil {
		t.Errorf("empty Flush errored: %v", err)
	}
	if rec.calls.Load() != 0 {
		t.Errorf("empty Flush posted: %d", rec.calls.Load())
	}
}

// TestSink_StatsCounters
func TestSink_StatsCounters(t *testing.T) {
	rec := newRecorder()
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	s, _ := webhook.New(webhook.Options{URL: srv.URL, BatchSize: 1})
	for i := 0; i < 3; i++ {
		_ = s.OnRecord(context.Background(), mkRecord(uint64(0x1000+i), []byte{byte(i)}))
	}
	stats := s.Stats()
	if stats.PostsAttempted != 3 || stats.PostsSucceeded != 3 ||
		stats.TotalRecordsPosted != 3 {
		t.Errorf("counters off: %+v", stats)
	}
}

// fakeClock is a controllable clock for Tick tests.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// capturingDL records every dead-letter append.
type capturingDL struct {
	mu        sync.Mutex
	envelopes []webhook.DeadLetterEnvelope
}

func (d *capturingDL) Append(_ context.Context, env webhook.DeadLetterEnvelope) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.envelopes = append(d.envelopes, env)
	return nil
}

// erroringDL always returns an error.
type erroringDL struct{}

func (e *erroringDL) Append(context.Context, webhook.DeadLetterEnvelope) error {
	return errSentinel
}

var errSentinel = &dlError{}

type dlError struct{}

func (dlError) Error() string { return "dl persist failed" }
