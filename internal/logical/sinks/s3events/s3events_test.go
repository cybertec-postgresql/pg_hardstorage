package s3events_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical/sinks/s3events"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/logicalreceiver"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// freshStorage spins up a fs-backed StoragePlugin rooted at a
// temp dir.  All sinks in the test suite share this shape.
func freshStorage(t *testing.T) (storage.StoragePlugin, string) {
	t.Helper()
	root := t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: "file://" + root}); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("file://" + root)
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp, root
}

// fixedNow returns a deterministic clock so tests can assert key
// shapes that include date-partitioning.
func fixedNow() func() time.Time {
	t := time.Date(2026, 5, 3, 14, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

// rec builds one logical record.
func rec(start, end pglogrepl.LSN, body string) logicalreceiver.Record {
	return logicalreceiver.Record{
		WALStart:     start,
		ServerWALEnd: end,
		ServerTime:   time.Date(2026, 5, 3, 14, 0, 0, 0, time.UTC),
		Data:         []byte(body),
	}
}

// ----- build-time validation -----

func TestNew_RequiresStorage(t *testing.T) {
	_, err := s3events.New(s3events.Options{Deployment: "d", Stream: "s"})
	if err == nil || !strings.Contains(err.Error(), "Storage") {
		t.Errorf("expected Storage error; got %v", err)
	}
}

func TestNew_RequiresDeployment(t *testing.T) {
	sp, _ := freshStorage(t)
	_, err := s3events.New(s3events.Options{Storage: sp, Stream: "s"})
	if err == nil || !strings.Contains(err.Error(), "Deployment") {
		t.Errorf("expected Deployment error; got %v", err)
	}
}

func TestNew_RequiresStream(t *testing.T) {
	sp, _ := freshStorage(t)
	_, err := s3events.New(s3events.Options{Storage: sp, Deployment: "d"})
	if err == nil || !strings.Contains(err.Error(), "Stream") {
		t.Errorf("expected Stream error; got %v", err)
	}
}

func TestNew_DefaultsPrefix(t *testing.T) {
	sp, _ := freshStorage(t)
	sink, err := s3events.New(s3events.Options{
		Storage: sp, Deployment: "db1", Stream: "events",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
}

// ----- emission -----

func TestOnRecord_BatchSizeFlush(t *testing.T) {
	sp, _ := freshStorage(t)
	sink, err := s3events.New(s3events.Options{
		Storage: sp, Deployment: "db1", Stream: "evt",
		BatchSize: 3, Now: fixedNow(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	for i := 0; i < 3; i++ {
		if err := sink.OnRecord(context.Background(),
			rec(pglogrepl.LSN(i*16), pglogrepl.LSN((i+1)*16), "rec")); err != nil {
			t.Fatalf("OnRecord %d: %v", i, err)
		}
	}
	batches, records, _, _ := sink.StatsSnapshot()
	if batches != 1 {
		t.Errorf("batches = %d, want 1", batches)
	}
	if records != 3 {
		t.Errorf("records = %d, want 3", records)
	}
}

func TestFlush_ExplicitFlush(t *testing.T) {
	sp, _ := freshStorage(t)
	sink, _ := s3events.New(s3events.Options{
		Storage: sp, Deployment: "db1", Stream: "evt",
		BatchSize: 100, Now: fixedNow(),
	})
	defer sink.Close()

	for i := 0; i < 5; i++ {
		_ = sink.OnRecord(context.Background(),
			rec(pglogrepl.LSN(i*16), pglogrepl.LSN((i+1)*16), "rec"))
	}
	if err := sink.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	batches, records, _, _ := sink.StatsSnapshot()
	if batches != 1 || records != 5 {
		t.Errorf("after Flush: batches=%d records=%d, want 1/5", batches, records)
	}
}

func TestFlush_EmptyIsNoOp(t *testing.T) {
	sp, _ := freshStorage(t)
	sink, _ := s3events.New(s3events.Options{
		Storage: sp, Deployment: "db1", Stream: "evt",
		Now: fixedNow(),
	})
	defer sink.Close()

	if err := sink.Flush(context.Background()); err != nil {
		t.Errorf("empty Flush returned %v; want nil", err)
	}
	batches, _, _, _ := sink.StatsSnapshot()
	if batches != 0 {
		t.Errorf("empty Flush wrote a batch")
	}
}

func TestTick_BatchInterval(t *testing.T) {
	now := time.Date(2026, 5, 3, 14, 0, 0, 0, time.UTC)
	clock := &controlledClock{now: now}
	sp, _ := freshStorage(t)
	sink, _ := s3events.New(s3events.Options{
		Storage: sp, Deployment: "db1", Stream: "evt",
		BatchSize:     1000,
		BatchInterval: time.Second,
		Now:           clock.Now,
	})
	defer sink.Close()

	_ = sink.OnRecord(context.Background(), rec(0, 16, "first"))

	// Tick before interval → no flush.
	if err := sink.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	batches, _, _, _ := sink.StatsSnapshot()
	if batches != 0 {
		t.Errorf("Tick before interval flushed; got %d batches", batches)
	}

	// Advance past interval → Tick flushes.
	clock.advance(2 * time.Second)
	if err := sink.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	batches, _, _, _ = sink.StatsSnapshot()
	if batches != 1 {
		t.Errorf("Tick after interval should flush; got %d batches", batches)
	}
}

// ----- key shape + envelope wire format -----

func TestEncode_ObjectKeyDatePartitioned(t *testing.T) {
	sp, _ := freshStorage(t)
	sink, _ := s3events.New(s3events.Options{
		Storage: sp, Deployment: "db1", Stream: "evt",
		Prefix:    "events/db1/evt",
		BatchSize: 1, Now: fixedNow(),
	})
	defer sink.Close()
	_ = sink.OnRecord(context.Background(), rec(0, 16, "x"))

	// fs storage stores under root/<key>; List the prefix and
	// look for the date-partitioned object.
	keys := listKeys(t, sp, "events/")
	if len(keys) == 0 {
		t.Fatal("no objects written")
	}
	want := "events/db1/evt/2026/05/03/14/"
	matched := false
	for _, k := range keys {
		if strings.HasPrefix(k, want) && strings.HasSuffix(k, ".json") {
			matched = true
		}
	}
	if !matched {
		t.Errorf("no object under %q in: %v", want, keys)
	}
}

func TestEncode_EnvelopeShape(t *testing.T) {
	sp, _ := freshStorage(t)
	sink, _ := s3events.New(s3events.Options{
		Storage: sp, Deployment: "db1", Stream: "evt", Slot: "slot1",
		BatchSize: 2, Now: fixedNow(),
	})
	defer sink.Close()

	_ = sink.OnRecord(context.Background(), rec(16, 32, "alpha"))
	_ = sink.OnRecord(context.Background(), rec(32, 48, "beta"))

	keys := listKeys(t, sp, "events/")
	if len(keys) != 1 {
		t.Fatalf("expected 1 object; got %d (%v)", len(keys), keys)
	}
	body := readKey(t, sp, keys[0])
	var env s3events.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, body)
	}
	if env.Schema != s3events.SinkSchema {
		t.Errorf("Schema = %q", env.Schema)
	}
	if env.Deployment != "db1" || env.Stream != "evt" || env.Slot != "slot1" {
		t.Errorf("identity fields drift: %+v", env)
	}
	if len(env.Records) != 2 {
		t.Errorf("Records len = %d, want 2", len(env.Records))
	}
	if env.StartLSN != 16 || env.EndLSN != 48 {
		t.Errorf("LSN bounds drift: start=%d end=%d", env.StartLSN, env.EndLSN)
	}
	if env.BatchID == "" {
		t.Errorf("BatchID empty")
	}
}

// TestEncode_DeterministicBatchID asserts identical input produces
// identical batch_id (idempotency contract).
func TestEncode_DeterministicBatchID(t *testing.T) {
	sp, _ := freshStorage(t)
	sink1, _ := s3events.New(s3events.Options{
		Storage: sp, Deployment: "db1", Stream: "evt",
		BatchSize: 2, Now: fixedNow(),
	})
	_ = sink1.OnRecord(context.Background(), rec(16, 32, "alpha"))
	_ = sink1.OnRecord(context.Background(), rec(32, 48, "beta"))
	sink1.Close()
	keys1 := listKeys(t, sp, "events/")

	// New sink, same records, same fixedNow — same key + body.
	sp2, _ := freshStorage(t)
	sink2, _ := s3events.New(s3events.Options{
		Storage: sp2, Deployment: "db1", Stream: "evt",
		BatchSize: 2, Now: fixedNow(),
	})
	_ = sink2.OnRecord(context.Background(), rec(16, 32, "alpha"))
	_ = sink2.OnRecord(context.Background(), rec(32, 48, "beta"))
	sink2.Close()
	keys2 := listKeys(t, sp2, "events/")

	if len(keys1) != 1 || len(keys2) != 1 {
		t.Fatalf("expected one object each; got %d / %d", len(keys1), len(keys2))
	}
	if keys1[0] != keys2[0] {
		t.Errorf("non-deterministic key: %s vs %s", keys1[0], keys2[0])
	}
}

// ----- syncedLSN -----

func TestSyncedLSN_AdvancesOnSuccessfulPut(t *testing.T) {
	sp, _ := freshStorage(t)
	sink, _ := s3events.New(s3events.Options{
		Storage: sp, Deployment: "db1", Stream: "evt",
		BatchSize: 1, Now: fixedNow(),
	})
	defer sink.Close()

	if got := sink.SyncedLSN(); got != 0 {
		t.Errorf("initial SyncedLSN = %d, want 0", got)
	}
	_ = sink.OnRecord(context.Background(), rec(16, 48, "x"))
	if got := sink.SyncedLSN(); got != 48 {
		t.Errorf("SyncedLSN after Put = %d, want 48", got)
	}
}

// ----- failure behaviours -----

// TestPutFailure_NoDeadLetter_StallsSlot: a Put failure with no
// dead-letter configured re-buffers the batch + advances neither
// stats nor syncedLSN.  Operator sees the slot stall.
func TestPutFailure_NoDeadLetter_StallsSlot(t *testing.T) {
	sp := &failingStorage{}
	sink, err := s3events.New(s3events.Options{
		Storage: sp, Deployment: "db1", Stream: "evt",
		BatchSize: 1, Now: fixedNow(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	err = sink.OnRecord(context.Background(), rec(16, 48, "x"))
	if err == nil {
		t.Errorf("expected Put failure to surface")
	}
	batches, _, _, stalled := sink.StatsSnapshot()
	if batches != 0 || stalled != 1 {
		t.Errorf("counters: batches=%d stalled=%d, want 0/1", batches, stalled)
	}
	if got := sink.SyncedLSN(); got != 0 {
		t.Errorf("SyncedLSN advanced past a failed put: %d", got)
	}
}

// TestPutFailure_WithDeadLetter_AdvancesSlot: when a dead-letter
// sink is wired, a Put failure routes the batch to the dead letter
// + advances syncedLSN (so the operator can recover out-of-band).
func TestPutFailure_WithDeadLetter_AdvancesSlot(t *testing.T) {
	primary := &failingStorage{}
	dl := &capturingDeadLetter{}
	sink, _ := s3events.New(s3events.Options{
		Storage:    primary,
		Deployment: "db1", Stream: "evt",
		BatchSize: 1, Now: fixedNow(),
		DeadLetter: dl,
	})
	defer sink.Close()
	err := sink.OnRecord(context.Background(), rec(16, 48, "x"))
	if err != nil {
		t.Errorf("expected nil error when dead-letter accepts; got %v", err)
	}
	if len(dl.envelopes) != 1 {
		t.Fatalf("expected 1 dead-letter envelope; got %d", len(dl.envelopes))
	}
	if got := sink.SyncedLSN(); got != 48 {
		t.Errorf("SyncedLSN should advance past dead-letter; got %d", got)
	}
	_, _, deadLettered, _ := sink.StatsSnapshot()
	if deadLettered != 1 {
		t.Errorf("BatchesDeadLettered = %d, want 1", deadLettered)
	}
}

// TestPutFailure_DeadLetterAlsoFails_StallsSlot: both backends
// failing reverts to the stall behaviour.  Operator gets a loud
// error from both.
func TestPutFailure_DeadLetterAlsoFails_StallsSlot(t *testing.T) {
	primary := &failingStorage{}
	dl := &failingDeadLetter{}
	sink, _ := s3events.New(s3events.Options{
		Storage:    primary,
		Deployment: "db1", Stream: "evt",
		BatchSize: 1, Now: fixedNow(),
		DeadLetter: dl,
	})
	defer sink.Close()
	err := sink.OnRecord(context.Background(), rec(16, 48, "x"))
	if err == nil {
		t.Errorf("expected combined put+dl failure to surface")
	}
	if got := sink.SyncedLSN(); got != 0 {
		t.Errorf("SyncedLSN advanced despite both failures: %d", got)
	}
	_, _, _, stalled := sink.StatsSnapshot()
	if stalled != 1 {
		t.Errorf("BatchesStalled = %d, want 1", stalled)
	}
}

// ----- StorageDeadLetter -----

func TestStorageDeadLetter_RoundTrip(t *testing.T) {
	sp, _ := freshStorage(t)
	dl := s3events.NewStorageDeadLetter(sp, "db1", "evt")
	env := s3events.DeadLetterEnvelope{
		Schema:     s3events.SinkSchema,
		BatchID:    "abc123",
		Deployment: "db1",
		Stream:     "evt",
	}
	if err := dl.WriteDeadLetter(context.Background(), env); err != nil {
		t.Fatalf("WriteDeadLetter: %v", err)
	}
	keys := listKeys(t, sp, "events/db1/evt/dead-letter/")
	if len(keys) != 1 {
		t.Fatalf("expected 1 dead-letter object; got %d", len(keys))
	}
	if !strings.HasSuffix(keys[0], "abc123.json") {
		t.Errorf("dead-letter key shape: %q", keys[0])
	}
}

// ----- ctx cancellation -----

func TestOnRecord_AfterClose_Errors(t *testing.T) {
	sp, _ := freshStorage(t)
	sink, _ := s3events.New(s3events.Options{
		Storage: sp, Deployment: "db1", Stream: "evt", Now: fixedNow(),
	})
	_ = sink.Close()
	err := sink.OnRecord(context.Background(), rec(0, 16, "x"))
	if err == nil {
		t.Errorf("expected error on closed sink")
	}
}

// ----- helpers -----

type controlledClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *controlledClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *controlledClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// failingStorage stubs StoragePlugin so every Put returns an
// error.  Lets us exercise the failure path without needing a
// real broken backend.
type failingStorage struct{ storage.NopBarrier }

func (f *failingStorage) Name() string                                      { return "failing" }
func (f *failingStorage) Open(context.Context, storage.StorageConfig) error { return nil }
func (f *failingStorage) Close() error                                      { return nil }
func (f *failingStorage) Capabilities() storage.Capabilities                { return storage.Capabilities{} }
func (f *failingStorage) Put(context.Context, string, io.Reader, storage.PutOptions) (storage.PutResult, error) {
	return storage.PutResult{}, errors.New("simulated put failure")
}
func (f *failingStorage) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("nope")
}
func (f *failingStorage) Stat(context.Context, string) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, errors.New("nope")
}
func (f *failingStorage) Delete(context.Context, string) error                    { return nil }
func (f *failingStorage) RenameIfNotExists(context.Context, string, string) error { return nil }
func (f *failingStorage) SetRetention(context.Context, string, time.Time, storage.WORMMode) error {
	return storage.ErrUnsupported
}
func (f *failingStorage) List(_ context.Context, _ string) iter.Seq2[storage.ObjectInfo, error] {
	return func(yield func(storage.ObjectInfo, error) bool) {}
}

// capturingDeadLetter records every envelope WriteDeadLetter sees.
type capturingDeadLetter struct {
	mu        sync.Mutex
	envelopes []s3events.DeadLetterEnvelope
}

func (c *capturingDeadLetter) WriteDeadLetter(_ context.Context, env s3events.DeadLetterEnvelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.envelopes = append(c.envelopes, env)
	return nil
}

type failingDeadLetter struct{}

func (f *failingDeadLetter) WriteDeadLetter(_ context.Context, _ s3events.DeadLetterEnvelope) error {
	return errors.New("dl-write-failed")
}

func listKeys(t *testing.T, sp storage.StoragePlugin, prefix string) []string {
	t.Helper()
	var out []string
	for info, err := range sp.List(context.Background(), prefix) {
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		out = append(out, info.Key)
	}
	return out
}

func readKey(t *testing.T, sp storage.StoragePlugin, key string) []byte {
	t.Helper()
	rd, err := sp.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	body, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
