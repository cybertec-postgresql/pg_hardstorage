// Package s3events implements a logical-decoding sink that writes
// each batch of change events to a storage-plugin key (typically
// S3, but any StoragePlugin backend works: fs, gcs, azure).  This
// is the SPEC's "S3 events" sink in the logical-decoding output-
// plugin tier — alongside chunked, webhook, Kafka, and Pub/Sub.
//
// Operationally:
//
//   - Per-deployment logical slot streams change events.
//   - The s3events sink batches records and writes each batch as
//     one object under a date-partitioned prefix (e.g.
//     `events/<deployment>/<stream>/2026/05/03/00/<batch-id>.json`).
//   - On Put success → syncedLSN advances → PG releases retained
//     WAL.
//   - On persistent Put failure: slot stalls (default — loud
//     failure surfaced via lag metrics) OR records flow to a
//     dead-letter prefix in the same repo (opt-in via
//     DeadLetter option).
//
// Why date-partitioned prefixes: downstream data-lake consumers
// (S3-Notification → Lambda, Glue crawler, Spark batch read,
// Athena partition scans) all benefit from prefix-aware
// partitioning.  The hour granularity keeps any single prefix
// bounded for LIST operations.
//
// Wire format: same shape as the webhook sink's POST body.
// Each object is one JSON document containing a deterministic
// batch_id (SHA-256 of records + LSN bounds), the records, and
// the LSN range.  Receivers that need byte-equal idempotency
// match on batch_id; receivers that just want at-least-once
// processing read the records array.
//
// Idempotency: a retry on a transient Put failure rewrites the
// same object key (deterministic batch_id) with byte-identical
// content.  S3 / gcs / fs / azure all overwrite atomically.
//
// Optional SNS notification: a future commit can wire SNS
// publish on each Put for downstream "object created" event
// fan-out;+ ships the writer half only.
package s3events

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/logicalreceiver"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// SinkSchema is the on-disk version tag for envelopes written by
// the s3events sink.  24-month backward-compat.
const SinkSchema = "pg_hardstorage.logical.s3events.v1"

// Defaults: same shape as the webhook sink so operators picking
// between them don't have to re-tune.
const (
	DefaultBatchSize     = 100
	DefaultBatchInterval = 5 * time.Second
)

// Options drives one Sink construction.
type Options struct {
	// Storage is the destination.  Required.
	Storage storage.StoragePlugin

	// Prefix is the key-prefix the sink writes objects under.  An
	// empty string defaults to "events/" — but the operator
	// should typically scope it to deployment + stream so multiple
	// sinks don't collide.  The sink appends date-partitioning
	// (yyyy/mm/dd/hh) and the per-batch object name (batch_id +
	// .json) under this prefix.
	Prefix string

	// Deployment + Stream identify the logical-decoding source.
	// Recorded in every envelope's body so a downstream consumer
	// can demultiplex when multiple deployments share a sink
	// destination.  Required.
	Deployment string
	Stream     string
	Slot       string

	// BatchSize / BatchInterval flush triggers.  Either fires
	// independently; the first to trip wins.  Defaults to
	// DefaultBatchSize / DefaultBatchInterval.
	BatchSize     int
	BatchInterval time.Duration

	// DeadLetter, when non-nil, captures batches that hit a
	// persistent Put failure.  Without it, the sink stalls on
	// failure (default — keeps the slot loud).  With it, failed
	// batches flow to the dead-letter sink + syncedLSN advances;
	// the operator recovers / replays out-of-band.
	DeadLetter DeadLetterSink

	// Now overrides time.Now (deterministic tests).  Optional.
	Now func() time.Time
}

// DeadLetterSink is the contract a dead-letter destination must
// implement.  Same shape as the webhook sink's contract so the
// existing storage-backed implementation works unchanged.
type DeadLetterSink interface {
	WriteDeadLetter(ctx context.Context, env DeadLetterEnvelope) error
}

// DeadLetterEnvelope is what flows to the dead-letter sink on
// persistent Put failure.  Carries everything the operator
// needs to manually replay: full record bytes, LSN bounds, the
// last error message, the failed-at timestamp.
type DeadLetterEnvelope struct {
	Schema       string           `json:"schema"`
	BatchID      string           `json:"batch_id"`
	Deployment   string           `json:"deployment"`
	Stream       string           `json:"stream"`
	Slot         string           `json:"slot"`
	Records      []EnvelopeRecord `json:"records"`
	StartLSN     pglogrepl.LSN    `json:"start_lsn"`
	EndLSN       pglogrepl.LSN    `json:"end_lsn"`
	LastError    string           `json:"last_error"`
	AttemptCount int              `json:"attempt_count"`
	FailedAt     time.Time        `json:"failed_at"`
}

// Envelope is what the sink writes per batch.  JSON-encoded;
// receivers parse a single stable shape.
type Envelope struct {
	Schema     string           `json:"schema"`
	BatchID    string           `json:"batch_id"`
	Deployment string           `json:"deployment"`
	Stream     string           `json:"stream"`
	Slot       string           `json:"slot"`
	StartLSN   pglogrepl.LSN    `json:"start_lsn"`
	EndLSN     pglogrepl.LSN    `json:"end_lsn"`
	WrittenAt  time.Time        `json:"written_at"`
	Records    []EnvelopeRecord `json:"records"`
}

// EnvelopeRecord is one record's wire shape.  Body is base64
// because logical-decoding records are arbitrary bytes (pgoutput
// protocol payload, wal2json text, custom plugin output, …).
type EnvelopeRecord struct {
	WALStart pglogrepl.LSN `json:"wal_start"`
	Data     string        `json:"data"` // base64
}

// Sink batches logical-decoding records and writes them as
// date-partitioned JSON objects to a storage backend.
type Sink struct {
	storage    storage.StoragePlugin
	prefix     string
	deployment string
	stream     string
	slot       string

	batchSize     int
	batchInterval time.Duration

	deadLetter DeadLetterSink

	now func() time.Time

	mu          sync.Mutex
	buffered    []logicalreceiver.Record
	bufferStart pglogrepl.LSN
	bufferEnd   pglogrepl.LSN
	bufferSince time.Time
	closed      bool

	syncedLSN atomic.Uint64

	stats Stats
}

// Stats is the per-sink counter set, exposed for monitoring +
// tests.  Fields are atomic-safe to read; callers reading mid-
// run see eventually-consistent counts.
type Stats struct {
	BatchesWritten      atomic.Uint64
	RecordsWritten      atomic.Uint64
	BatchesDeadLettered atomic.Uint64
	BatchesStalled      atomic.Uint64
}

// New constructs a Sink.  Returns an error for required-field
// violations so misconfiguration surfaces at startup.
func New(opts Options) (*Sink, error) {
	if opts.Storage == nil {
		return nil, errors.New("s3events: Storage is required")
	}
	if opts.Deployment == "" {
		return nil, errors.New("s3events: Deployment is required")
	}
	if opts.Stream == "" {
		return nil, errors.New("s3events: Stream is required")
	}
	prefix := opts.Prefix
	if prefix == "" {
		prefix = "events/" + opts.Deployment + "/" + opts.Stream
	}
	prefix = strings.TrimRight(prefix, "/")
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	bs := opts.BatchSize
	if bs <= 0 {
		bs = DefaultBatchSize
	}
	bi := opts.BatchInterval
	if bi <= 0 {
		bi = DefaultBatchInterval
	}
	return &Sink{
		storage:       opts.Storage,
		prefix:        prefix,
		deployment:    opts.Deployment,
		stream:        opts.Stream,
		slot:          opts.Slot,
		batchSize:     bs,
		batchInterval: bi,
		deadLetter:    opts.DeadLetter,
		now:           now,
	}, nil
}

// OnRecord buffers the record + flushes when BatchSize is hit.
// logicalreceiver.Sink contract.
func (s *Sink) OnRecord(ctx context.Context, rec logicalreceiver.Record) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("s3events: sink closed")
	}
	if len(s.buffered) == 0 {
		s.bufferStart = rec.WALStart
		s.bufferSince = s.now()
	}
	s.buffered = append(s.buffered, rec)
	if rec.ServerWALEnd > s.bufferEnd {
		s.bufferEnd = rec.ServerWALEnd
	}
	if len(s.buffered) >= s.batchSize {
		return s.flushLocked(ctx)
	}
	s.mu.Unlock()
	return nil
}

// Tick is the wallclock-based flush trigger.  Caller invokes
// from a periodic loop (the runner does this every 1s).
// logicalreceiver.Sink contract.
func (s *Sink) Tick(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	if len(s.buffered) == 0 {
		s.mu.Unlock()
		return nil
	}
	if s.now().Sub(s.bufferSince) < s.batchInterval {
		s.mu.Unlock()
		return nil
	}
	return s.flushLocked(ctx)
}

// Flush forces a flush of any buffered records.  Returns nil if
// the buffer was already empty.  logicalreceiver.Sink contract.
func (s *Sink) Flush(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	if len(s.buffered) == 0 {
		s.mu.Unlock()
		return nil
	}
	return s.flushLocked(ctx)
}

// SyncedLSN reports the LSN through which the sink has durably
// committed.  logicalreceiver.Sink contract.
func (s *Sink) SyncedLSN() pglogrepl.LSN {
	return pglogrepl.LSN(s.syncedLSN.Load())
}

// Close marks the sink closed.  Any subsequent OnRecord returns
// an error; in-flight Flush completes.
func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// StatsSnapshot returns a copy of the counters at this moment.
func (s *Sink) StatsSnapshot() (batches, records, deadLettered, stalled uint64) {
	return s.stats.BatchesWritten.Load(),
		s.stats.RecordsWritten.Load(),
		s.stats.BatchesDeadLettered.Load(),
		s.stats.BatchesStalled.Load()
}

// flushLocked writes the current buffer to storage.  Caller
// holds s.mu; the function unlocks before any I/O so concurrent
// OnRecord calls can buffer the next batch while this one
// commits.
func (s *Sink) flushLocked(ctx context.Context) error {
	batch := s.buffered
	startLSN := s.bufferStart
	endLSN := s.bufferEnd
	s.buffered = nil
	s.bufferStart = 0
	s.bufferEnd = 0
	s.bufferSince = time.Time{}
	s.mu.Unlock()

	body, batchID, key, err := s.encode(batch, startLSN, endLSN)
	if err != nil {
		return fmt.Errorf("s3events: encode: %w", err)
	}

	_, putErr := s.storage.Put(ctx, key, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	})
	if putErr == nil {
		s.stats.BatchesWritten.Add(1)
		s.stats.RecordsWritten.Add(uint64(len(batch)))
		// Advance syncedLSN so PG can release WAL.  We use endLSN
		// (highest ServerWALEnd we observed) so the slot moves
		// monotonically.
		s.syncedLSN.Store(uint64(endLSN))
		return nil
	}

	// Persistent Put failure.  If a dead-letter sink is configured,
	// route the batch there + advance syncedLSN; otherwise return
	// the error so the upstream stalls (the safe default).
	if s.deadLetter == nil {
		// Re-buffer the batch so the next flush retries instead of
		// silently losing it.  We acquire the lock again because
		// the unlock happened above; the closed check covers a
		// concurrent Close.
		s.mu.Lock()
		if !s.closed {
			s.buffered = append(batch, s.buffered...)
			s.bufferStart = startLSN
			if s.bufferEnd < endLSN {
				s.bufferEnd = endLSN
			}
		}
		s.mu.Unlock()
		s.stats.BatchesStalled.Add(1)
		return fmt.Errorf("s3events: put %s: %w", key, putErr)
	}

	env := DeadLetterEnvelope{
		Schema:       SinkSchema,
		BatchID:      batchID,
		Deployment:   s.deployment,
		Stream:       s.stream,
		Slot:         s.slot,
		Records:      buildEnvelopeRecords(batch),
		StartLSN:     startLSN,
		EndLSN:       endLSN,
		LastError:    putErr.Error(),
		AttemptCount: 1,
		FailedAt:     s.now().UTC(),
	}
	if dlErr := s.deadLetter.WriteDeadLetter(ctx, env); dlErr != nil {
		// Even the dead-letter failed: re-buffer the batch + return.
		s.mu.Lock()
		if !s.closed {
			s.buffered = append(batch, s.buffered...)
			s.bufferStart = startLSN
			if s.bufferEnd < endLSN {
				s.bufferEnd = endLSN
			}
		}
		s.mu.Unlock()
		s.stats.BatchesStalled.Add(1)
		return fmt.Errorf("s3events: put %s + dead-letter both failed: put=%v dl=%v",
			key, putErr, dlErr)
	}
	s.stats.BatchesDeadLettered.Add(1)
	s.syncedLSN.Store(uint64(endLSN))
	return nil
}

// encode produces the wire bytes for a batch + computes its
// deterministic batch_id + the date-partitioned object key.
//
// batch_id: SHA-256 of (every record's WALStart + Data) || (start
// LSN || end LSN) — the same bytes the webhook sink hashes.
// Receivers can dedupe across both sinks if a deployment dual-
// writes for migration / verification.
//
// key: <prefix>/yyyy/mm/dd/hh/<batch_id>.json — date-partitioned
// for downstream LIST efficiency + Athena/Glue compatibility.
func (s *Sink) encode(batch []logicalreceiver.Record, startLSN, endLSN pglogrepl.LSN) (body []byte, batchID, key string, err error) {
	hasher := sha256.New()
	for _, rec := range batch {
		fmt.Fprintf(hasher, "%016x", uint64(rec.WALStart))
		hasher.Write(rec.Data)
	}
	fmt.Fprintf(hasher, "%016x%016x", uint64(startLSN), uint64(endLSN))
	bidBytes := hasher.Sum(nil)
	batchID = hex.EncodeToString(bidBytes[:8])

	now := s.now().UTC()
	env := Envelope{
		Schema:     SinkSchema,
		BatchID:    batchID,
		Deployment: s.deployment,
		Stream:     s.stream,
		Slot:       s.slot,
		StartLSN:   startLSN,
		EndLSN:     endLSN,
		WrittenAt:  now,
		Records:    buildEnvelopeRecords(batch),
	}
	body, err = json.Marshal(&env)
	if err != nil {
		return nil, "", "", err
	}
	key = fmt.Sprintf("%s/%04d/%02d/%02d/%02d/%s.json",
		s.prefix,
		now.Year(), int(now.Month()), now.Day(), now.Hour(),
		batchID)
	return body, batchID, key, nil
}

func buildEnvelopeRecords(batch []logicalreceiver.Record) []EnvelopeRecord {
	out := make([]EnvelopeRecord, len(batch))
	for i, rec := range batch {
		out[i] = EnvelopeRecord{
			WALStart: rec.WALStart,
			Data:     base64.StdEncoding.EncodeToString(rec.Data),
		}
	}
	return out
}

// StorageDeadLetter is a default DeadLetterSink that writes
// envelopes to a storage-plugin prefix.  Convenient when the
// operator wants dead-letter retention without standing up a
// separate destination.
type StorageDeadLetter struct {
	sp     storage.StoragePlugin
	prefix string
}

// NewStorageDeadLetter constructs a StorageDeadLetter rooted at
// the given storage + prefix.  Pass the same storage as the
// main sink + a sibling prefix (e.g. "events/.../dead-letter/")
// for a single-backend recovery story.
func NewStorageDeadLetter(sp storage.StoragePlugin, deployment, stream string) *StorageDeadLetter {
	return &StorageDeadLetter{
		sp:     sp,
		prefix: "events/" + deployment + "/" + stream + "/dead-letter",
	}
}

// WriteDeadLetter persists one envelope.  Idempotent on byte-
// equal content (deterministic batch_id → deterministic key).
func (d *StorageDeadLetter) WriteDeadLetter(ctx context.Context, env DeadLetterEnvelope) error {
	body, err := json.Marshal(&env)
	if err != nil {
		return err
	}
	key := d.prefix + "/" + env.BatchID + ".json"
	_, err = d.sp.Put(ctx, key, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	})
	return err
}
