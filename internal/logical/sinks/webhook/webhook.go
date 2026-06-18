// Package webhook implements a logical-decoding sink that POSTs
// each record (or batch of records) to a configurable HTTPS
// endpoint.  This is the SPEC's "webhook" sink in the logical-
// decoding output-plugin tier.
//
// Operationally:
//
//   - Per-deployment logical slot streams change events.
//   - Webhook sink batches records + POSTs to the operator's
//     downstream system (Kafka REST proxy, Pub/Sub HTTP, custom
//     event bus, etc).
//   - On POST success, syncedLSN advances → PG releases retained
//     WAL.
//   - On persistent POST failure: slot stalls (default) OR
//     records flow to a dead-letter prefix in the repo (opt-in
//     via DeadLetter option).  The default is the safe choice —
//     operators want a stalled slot to be loud.
//
// Retry policy: exponential backoff with jitter, capped at
// RetryBudget attempts per batch.  Idempotency is the receiver's
// responsibility — we send the same batch on retry, so the
// receiver SHOULD dedupe by content hash or LSN.  The default
// X-PG-Hardstorage-Batch-ID header carries a deterministic
// per-batch UUID for that purpose.
package webhook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/logicalreceiver"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// SinkSchema is the on-disk version tag for any persistence
// (dead-letter envelopes) the webhook sink writes.
const SinkSchema = "pg_hardstorage.logical.webhook.v1"

// DefaultBatchSize is the records-per-POST default.  Small
// enough that a single 500 doesn't burn an hour of work; big
// enough that small databases don't drown the receiver in
// 1-record requests.
const DefaultBatchSize = 100

// DefaultBatchInterval is the wallclock-based flush deadline.
// Records buffered for longer than this without filling a batch
// get POSTed anyway.
const DefaultBatchInterval = 5 * time.Second

// DefaultTimeout is the per-POST HTTP timeout.  Generous enough
// for slow receivers; short enough that a stuck receiver
// doesn't block the receive loop indefinitely.
const DefaultTimeout = 30 * time.Second

// DefaultRetryBudget is the retry count per batch before the
// sink gives up.  Five matches the SPEC's "transient retry
// budget" (5xx, 429, network errors).  4xx errors are not
// retried — they're client-side bugs and exhaust the budget
// immediately.
const DefaultRetryBudget = 5

// DefaultRetryBaseDelay is the first-retry delay; subsequent
// retries double up to RetryMaxDelay.
const DefaultRetryBaseDelay = 1 * time.Second

// DefaultRetryMaxDelay caps the exponential backoff.  60s is
// long enough that a receiver with a 30s outage recovers
// without burning the budget.
const DefaultRetryMaxDelay = 60 * time.Second

// Options configures a webhook Sink.
type Options struct {
	// URL is the destination POST endpoint.  Required.
	URL string

	// Authorization, when non-empty, is the value of the
	// `Authorization` header on every POST (e.g. "Bearer ABC").
	// Optional.
	Authorization string

	// HTTPClient is the underlying client.  Defaults to a
	// dedicated `&http.Client{Timeout: opts.Timeout}` constructed
	// in New — NOT http.DefaultClient (which has no timeout and
	// would leak goroutines on slow / hung receivers).  Tests
	// inject a custom transport to capture / mock requests.
	HTTPClient *http.Client

	// BatchSize is the records-per-POST cap.  Default
	// DefaultBatchSize.
	BatchSize int

	// BatchInterval is the wallclock-based flush deadline.
	// Default DefaultBatchInterval.
	BatchInterval time.Duration

	// Timeout is the per-POST HTTP timeout.  Default
	// DefaultTimeout.
	Timeout time.Duration

	// RetryBudget is the max retry count per batch.  Default
	// DefaultRetryBudget.
	RetryBudget int

	// RetryBaseDelay is the first-retry backoff.  Default
	// DefaultRetryBaseDelay.
	RetryBaseDelay time.Duration

	// RetryMaxDelay caps the exponential backoff.  Default
	// DefaultRetryMaxDelay.
	RetryMaxDelay time.Duration

	// DeadLetter, when non-nil, captures batches that exhaust
	// their retry budget.  When DeadLetter is nil, exhaustion
	// stalls the sink (syncedLSN doesn't advance) — the safe
	// default for operators who want failures to be loud.
	DeadLetter DeadLetterSink

	// Deployment + StreamName + Slot are recorded in HTTP
	// headers + dead-letter envelopes so the receiver can
	// route by source.
	Deployment string
	StreamName string
	Slot       string

	// Now overrides time.Now() for deterministic test output.
	Now func() time.Time

	// Sleep is the test hook that overrides time.Sleep in the
	// retry path.  Default = time.Sleep.  Tests inject a
	// no-op stub to drive the retry loop synchronously.
	Sleep func(time.Duration)
}

// DeadLetterSink is the optional last-resort consumer for
// batches that exhaust their retry budget.
type DeadLetterSink interface {
	// Append is called once per exhausted batch.  Implementations
	// must NOT block the receive loop indefinitely; they're
	// expected to write to durable storage + return promptly.
	Append(ctx context.Context, env DeadLetterEnvelope) error
}

// DeadLetterEnvelope is the persistence shape for a dead-letter
// batch.  Records are the original logicalreceiver.Records;
// LastError is the final HTTP / network error; Attempts is the
// count of attempts made before giving up.
type DeadLetterEnvelope struct {
	Schema     string                   `json:"schema"`
	BatchID    string                   `json:"batch_id"`
	Deployment string                   `json:"deployment,omitempty"`
	StreamName string                   `json:"stream_name,omitempty"`
	Slot       string                   `json:"slot,omitempty"`
	Records    []logicalreceiver.Record `json:"records"`
	StartLSN   pglogrepl.LSN            `json:"start_lsn"`
	EndLSN     pglogrepl.LSN            `json:"end_lsn"`
	LastError  string                   `json:"last_error"`
	Attempts   int                      `json:"attempts"`
	FailedAt   time.Time                `json:"failed_at"`
}

// StorageDeadLetter writes envelopes to the repo at
// `logical/<deployment>/<stream>/dead-letter/<batch-id>.json`.
// Operators reading the dead-letter prefix recover or replay
// the failed batches out-of-band.
type StorageDeadLetter struct {
	sp     storage.StoragePlugin
	prefix string
}

// NewStorageDeadLetter returns a DeadLetter that writes to sp
// under the canonical `logical/<deployment>/<stream>/dead-letter/`
// prefix.
func NewStorageDeadLetter(sp storage.StoragePlugin, deployment, stream string) *StorageDeadLetter {
	if deployment == "" {
		deployment = "default"
	}
	if stream == "" {
		stream = "default"
	}
	return &StorageDeadLetter{
		sp:     sp,
		prefix: fmt.Sprintf("logical/%s/%s/dead-letter/", deployment, stream),
	}
}

// Append writes the envelope as JSON.
func (d *StorageDeadLetter) Append(ctx context.Context, env DeadLetterEnvelope) error {
	body, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Errorf("webhook dead-letter: marshal: %w", err)
	}
	key := d.prefix + env.BatchID + ".json"
	if _, err := d.sp.Put(ctx, key, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	}); err != nil {
		return fmt.Errorf("webhook dead-letter: put %q: %w", key, err)
	}
	return nil
}

// Sink is the webhook logical-decoding sink.  Implements
// logicalreceiver.Sink + adds Tick / Flush / Close for
// orchestration parity with the chunked sink.
type Sink struct {
	opts Options

	mu       sync.Mutex
	buf      []logicalreceiver.Record
	startLSN pglogrepl.LSN
	endLSN   pglogrepl.LSN

	syncedLSN atomic.Uint64
	lastFlush atomic.Int64

	// Diagnostic counters; surfaced via Stats() for the
	// orchestration layer + observability.
	postsAttempted      atomic.Int64
	postsSucceeded      atomic.Int64
	batchesDeadLettered atomic.Int64
	totalRecordsPosted  atomic.Int64
}

// New returns a sink ready to receive records.  Validates
// required fields + applies defaults for the rest.
func New(opts Options) (*Sink, error) {
	if opts.URL == "" {
		return nil, errors.New("webhook: URL is required")
	}
	if !strings.HasPrefix(opts.URL, "https://") && !strings.HasPrefix(opts.URL, "http://") {
		return nil, fmt.Errorf("webhook: URL must start with http:// or https://; got %q", opts.URL)
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBatchSize
	}
	if opts.BatchInterval <= 0 {
		opts.BatchInterval = DefaultBatchInterval
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.RetryBudget <= 0 {
		opts.RetryBudget = DefaultRetryBudget
	}
	if opts.RetryBaseDelay <= 0 {
		opts.RetryBaseDelay = DefaultRetryBaseDelay
	}
	if opts.RetryMaxDelay <= 0 {
		opts.RetryMaxDelay = DefaultRetryMaxDelay
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: opts.Timeout}
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Sleep == nil {
		opts.Sleep = time.Sleep
	}
	s := &Sink{opts: opts}
	s.lastFlush.Store(opts.Now().UnixNano())
	return s, nil
}

// OnRecord buffers the record + flushes when BatchSize is
// reached.  Returns the receive loop's error from flush; on
// flush success returns nil even if batches were
// dead-lettered.
func (s *Sink) OnRecord(ctx context.Context, rec logicalreceiver.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) == 0 {
		s.startLSN = rec.WALStart
	}
	s.endLSN = rec.WALStart + pglogrepl.LSN(len(rec.Data))
	s.buf = append(s.buf, rec)
	if len(s.buf) >= s.opts.BatchSize {
		return s.flushLocked(ctx)
	}
	return nil
}

// Tick checks BatchInterval and flushes if elapsed.  Cheap when
// nothing is buffered.
func (s *Sink) Tick(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) == 0 {
		return nil
	}
	last := time.Unix(0, s.lastFlush.Load())
	if s.opts.Now().Sub(last) < s.opts.BatchInterval {
		return nil
	}
	return s.flushLocked(ctx)
}

// Flush forces a flush of any buffered records.  Returns the
// receive-loop error from the POST + retries; on dead-letter,
// the records are persisted but Flush returns nil (the slot
// can advance).
func (s *Sink) Flush(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) == 0 {
		return nil
	}
	return s.flushLocked(ctx)
}

// SyncedLSN returns the last successfully-POSTed batch's
// EndLSN.  Read by the receive loop's status ticker.
func (s *Sink) SyncedLSN() pglogrepl.LSN {
	return pglogrepl.LSN(s.syncedLSN.Load())
}

// Stats returns the diagnostic counters.  Cheap to call.
func (s *Sink) Stats() Stats {
	return Stats{
		PostsAttempted:      s.postsAttempted.Load(),
		PostsSucceeded:      s.postsSucceeded.Load(),
		BatchesDeadLettered: s.batchesDeadLettered.Load(),
		TotalRecordsPosted:  s.totalRecordsPosted.Load(),
	}
}

// Stats is the per-sink diagnostic snapshot.
type Stats struct {
	PostsAttempted      int64 `json:"posts_attempted"`
	PostsSucceeded      int64 `json:"posts_succeeded"`
	BatchesDeadLettered int64 `json:"batches_dead_lettered,omitempty"`
	TotalRecordsPosted  int64 `json:"total_records_posted"`
}

// flushLocked is the inner flush.  Caller MUST hold s.mu.  On
// completion the buffer is cleared regardless of POST outcome
// (dead-lettered batches are released; persistent failure with
// no DeadLetter is reflected in syncedLSN NOT advancing).
func (s *Sink) flushLocked(ctx context.Context) error {
	batch := s.buf
	startLSN := s.startLSN
	endLSN := s.endLSN
	s.buf = nil
	s.startLSN = 0
	s.endLSN = 0
	s.lastFlush.Store(s.opts.Now().UnixNano())

	if len(batch) == 0 {
		return nil
	}

	batchID := computeBatchID(batch, startLSN, endLSN)
	body, err := encodeBatch(batch, batchID, startLSN, endLSN, s.opts)
	if err != nil {
		return fmt.Errorf("webhook: encode batch: %w", err)
	}

	postErr := s.postWithRetries(ctx, body, batchID)
	if postErr == nil {
		s.syncedLSN.Store(uint64(endLSN))
		s.postsSucceeded.Add(1)
		s.totalRecordsPosted.Add(int64(len(batch)))
		return nil
	}

	// POST exhausted retries.  Either dead-letter or stall.
	if s.opts.DeadLetter == nil {
		// No dead-letter: slot stalls.  The receive loop sees
		// the error + the operator notices via lag metrics.
		// Restore the buffer so a manual retry has the
		// records (this is a lossy choice — we drop the slot
		// advance but keep the records in memory; on agent
		// restart they're re-streamed by PG).
		s.buf = batch
		s.startLSN = startLSN
		s.endLSN = endLSN
		return fmt.Errorf("webhook: batch %s exhausted retry budget; slot will stall: %w",
			batchID, postErr)
	}

	// Dead-letter the batch + advance the slot.
	env := DeadLetterEnvelope{
		Schema:     SinkSchema,
		BatchID:    batchID,
		Deployment: s.opts.Deployment,
		StreamName: s.opts.StreamName,
		Slot:       s.opts.Slot,
		Records:    batch,
		StartLSN:   startLSN,
		EndLSN:     endLSN,
		LastError:  postErr.Error(),
		Attempts:   s.opts.RetryBudget,
		FailedAt:   s.opts.Now(),
	}
	if err := s.opts.DeadLetter.Append(ctx, env); err != nil {
		// Dead-letter itself failed.  This is bad; surface
		// AND stall the slot (don't advance + leave records
		// in buffer).
		s.buf = batch
		s.startLSN = startLSN
		s.endLSN = endLSN
		return fmt.Errorf("webhook: batch %s POST failed AND dead-letter persist failed: post=%v; dl=%w",
			batchID, postErr, err)
	}
	s.batchesDeadLettered.Add(1)
	s.syncedLSN.Store(uint64(endLSN))
	return nil
}

// postWithRetries performs the POST + retries with exponential
// backoff up to RetryBudget attempts.  Returns nil on success;
// the last attempt's error on exhaustion.
func (s *Sink) postWithRetries(ctx context.Context, body []byte, batchID string) error {
	var lastErr error
	delay := s.opts.RetryBaseDelay
	for attempt := 0; attempt < s.opts.RetryBudget; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.postsAttempted.Add(1)
		err := s.postOnce(ctx, body, batchID)
		if err == nil {
			return nil
		}
		lastErr = err
		// Permanent errors (4xx other than 408 + 429) exhaust
		// the budget immediately — no point retrying a
		// client-side bug.
		if isPermanentHTTPError(err) {
			return err
		}
		if attempt == s.opts.RetryBudget-1 {
			break
		}
		// Sleep with cap.
		if delay > s.opts.RetryMaxDelay {
			delay = s.opts.RetryMaxDelay
		}
		s.opts.Sleep(delay)
		delay *= 2
	}
	return lastErr
}

// postOnce performs one POST attempt.  Returns nil on 2xx;
// httpError otherwise.
func (s *Sink) postOnce(ctx context.Context, body []byte, batchID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.opts.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-PG-Hardstorage-Batch-ID", batchID)
	if s.opts.Deployment != "" {
		req.Header.Set("X-PG-Hardstorage-Deployment", s.opts.Deployment)
	}
	if s.opts.StreamName != "" {
		req.Header.Set("X-PG-Hardstorage-Stream", s.opts.StreamName)
	}
	if s.opts.Authorization != "" {
		req.Header.Set("Authorization", s.opts.Authorization)
	}
	resp, err := s.opts.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	// Drain the body so the connection can be reused (HTTP
	// keep-alive).  Cap the drain so a giant response from a
	// misbehaving receiver doesn't exhaust memory.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &httpError{statusCode: resp.StatusCode}
}

// httpError carries the response status code so the caller can
// classify retry-eligible vs permanent.
type httpError struct{ statusCode int }

// Error returns a "http <code>" representation of the captured
// response status.
func (e *httpError) Error() string {
	return fmt.Sprintf("http %d", e.statusCode)
}

// StatusCode exposes the captured response code.
func (e *httpError) StatusCode() int { return e.statusCode }

// isPermanentHTTPError classifies a POST error as exhaust-budget-
// immediately.  4xx EXCEPT 408 (timeout) + 429 (too many
// requests) are permanent.  Network errors are transient.
func isPermanentHTTPError(err error) bool {
	var he *httpError
	if !errors.As(err, &he) {
		return false
	}
	if he.statusCode >= 400 && he.statusCode < 500 {
		// 408 Request Timeout + 429 Too Many Requests are
		// transient — retry.
		if he.statusCode == 408 || he.statusCode == 429 {
			return false
		}
		return true
	}
	return false
}

// encodeBatch produces the wire bytes for one POST.  Format:
//
//	{
//	  "schema": "pg_hardstorage.logical.webhook.v1",
//	  "batch_id": "<id>",
//	  "deployment": "...", "stream_name": "...", "slot": "...",
//	  "start_lsn": "0/3000028", "end_lsn": "0/30001A0",
//	  "records": [
//	    { "wal_start": "0/3000028", "server_wal_end": "...",
//	      "server_time": "...", "data": "<base64>" },
//	    ...
//	  ]
//	}
//
// The receiver dedupes by batch_id.  Records are decoded
// downstream — the sink doesn't impose a wire format on the
// PG record bytes (that's the slot's plugin choice: pgoutput,
// wal2json, etc).
func encodeBatch(records []logicalreceiver.Record, batchID string, start, end pglogrepl.LSN, opts Options) ([]byte, error) {
	type wireRecord struct {
		WALStart     string    `json:"wal_start"`
		ServerWALEnd string    `json:"server_wal_end"`
		ServerTime   time.Time `json:"server_time"`
		Data         []byte    `json:"data"`
	}
	type wireBatch struct {
		Schema     string       `json:"schema"`
		BatchID    string       `json:"batch_id"`
		Deployment string       `json:"deployment,omitempty"`
		StreamName string       `json:"stream_name,omitempty"`
		Slot       string       `json:"slot,omitempty"`
		StartLSN   string       `json:"start_lsn"`
		EndLSN     string       `json:"end_lsn"`
		Records    []wireRecord `json:"records"`
	}
	wr := make([]wireRecord, 0, len(records))
	for _, r := range records {
		wr = append(wr, wireRecord{
			WALStart:     r.WALStart.String(),
			ServerWALEnd: r.ServerWALEnd.String(),
			ServerTime:   r.ServerTime,
			Data:         r.Data,
		})
	}
	wb := wireBatch{
		Schema:     SinkSchema,
		BatchID:    batchID,
		Deployment: opts.Deployment,
		StreamName: opts.StreamName,
		Slot:       opts.Slot,
		StartLSN:   start.String(),
		EndLSN:     end.String(),
		Records:    wr,
	}
	return json.Marshal(wb)
}

// computeBatchID returns a deterministic per-batch ID.  Used
// for idempotency: a retry of the same batch carries the same
// ID; the receiver dedupes.
func computeBatchID(records []logicalreceiver.Record, start, end pglogrepl.LSN) string {
	h := sha256.New()
	for _, r := range records {
		fmt.Fprintf(h, "%s|%d|", r.WALStart, len(r.Data))
		h.Write(r.Data)
	}
	fmt.Fprintf(h, "%s|%s", start, end)
	return hex.EncodeToString(h.Sum(nil)[:16])
}
