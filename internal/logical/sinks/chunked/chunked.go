// Package chunked is the v0.1 logical-decoding sink. It batches
// inbound XLogData payloads into segment-sized blobs and commits each
// batch into the repository as a content-addressed manifest.
//
// On-disk layout:
//
//	logical/<deployment>/<stream-name>/<start-lsn>.json
//
// Each manifest captures: the start/end LSN of the batch, the slot
// name + plugin, and the chunk references whose concatenation is the
// raw protocol byte stream.
//
// We deliberately store raw plugin bytes — for pgoutput that's the
// V1/V2 binary protocol. layers a decoder + per-table fanout on
// top; v0.1 is the durable-sink-with-resume baseline.
package chunked

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/chunker"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/logicalreceiver"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// Schema for the per-batch manifest. 24-month back-compat per the
// project-wide commitment.
const Schema = "pg_hardstorage.logical_segment.v1"

// DefaultBatchBytes targets a chunker-friendly segment size: 16 MiB
// matches the physical-WAL segment so a batch chunks at the same
// granularity as `wal stream`.
const DefaultBatchBytes = 16 * 1024 * 1024

// DefaultBatchInterval forces a flush even when traffic is sparse.
// Without it, a low-volume publication could buffer forever and
// confirmed_flush_lsn would never advance — PG would retain WAL
// indefinitely. 5s is a reasonable compromise between flush frequency
// and per-batch overhead.
const DefaultBatchInterval = 5 * time.Second

// SegmentManifest is the JSON body persisted per batch.
type SegmentManifest struct {
	Schema     string     `json:"schema"`
	Deployment string     `json:"deployment"`
	StreamName string     `json:"stream_name"`
	Slot       string     `json:"slot"`
	Plugin     string     `json:"plugin"`
	StartLSN   string     `json:"start_lsn"`
	EndLSN     string     `json:"end_lsn"`
	Records    int        `json:"records"`
	BytesIn    int64      `json:"bytes_in"`
	Chunks     []ChunkRef `json:"chunks"`
	CreatedAt  time.Time  `json:"created_at"`
}

// ChunkRef is one CAS reference; structurally identical to the
// physical WAL sink's chunk ref but typed separately so a future
// schema evolution doesn't drag the physical schema with it.
type ChunkRef struct {
	Hash   repo.Hash `json:"hash"`
	Offset int64     `json:"offset"`
	Len    int64     `json:"len"`
}

// Options configures a Sink.
type Options struct {
	Deployment string
	StreamName string
	Slot       string
	Plugin     string

	// BatchBytes is the soft byte limit for one segment. The current
	// batch flushes the moment a record would push past this; small
	// records past the threshold land in the next segment.
	BatchBytes int

	// BatchInterval forces a flush even when records are below
	// BatchBytes. 0 → DefaultBatchInterval. Set to a long duration
	// for high-throughput streams where wallclock-flushing would
	// fragment.
	BatchInterval time.Duration

	// ChunkerFactory builds the chunker per batch. Defaults to
	// chunker.New (FastCDC). Tests substitute fixed-size chunkers.
	ChunkerFactory func() *chunker.Chunker
}

// Sink is the v0.1 chunked logical-decoding sink.
type Sink struct {
	cas  *repo.CAS
	sp   storage.StoragePlugin
	opts Options

	mu       sync.Mutex
	buf      bytes.Buffer
	records  int
	startLSN pglogrepl.LSN // start LSN of the in-flight batch
	endLSN   pglogrepl.LSN // last record's WALStart + len(Data)

	// syncedLSN is the EndLSN of the most-recently-committed batch.
	// Read by SyncedLSN under no lock (atomic) so the receive loop's
	// status ticker doesn't fight Flush for the mutex.
	syncedLSN atomic.Uint64

	// lastFlush is the wall-clock instant of the most recent flush.
	// Used by Tick to decide whether the BatchInterval has elapsed.
	lastFlush atomic.Int64
}

// New returns a sink ready to receive records. cas + sp are not
// closed by the sink; the caller manages their lifecycle.
func New(cas *repo.CAS, sp storage.StoragePlugin, opts Options) (*Sink, error) {
	if cas == nil {
		return nil, errors.New("chunked: nil CAS")
	}
	if sp == nil {
		return nil, errors.New("chunked: nil StoragePlugin")
	}
	if opts.Deployment == "" {
		return nil, errors.New("chunked: empty deployment")
	}
	if opts.StreamName == "" {
		return nil, errors.New("chunked: empty stream name")
	}
	if opts.Slot == "" {
		return nil, errors.New("chunked: empty slot")
	}
	if opts.Plugin == "" {
		opts.Plugin = "pgoutput"
	}
	if opts.BatchBytes <= 0 {
		opts.BatchBytes = DefaultBatchBytes
	}
	if opts.BatchInterval <= 0 {
		opts.BatchInterval = DefaultBatchInterval
	}
	if opts.ChunkerFactory == nil {
		opts.ChunkerFactory = chunker.New
	}
	s := &Sink{cas: cas, sp: sp, opts: opts}
	s.lastFlush.Store(time.Now().UnixNano())
	return s, nil
}

// OnRecord implements logicalreceiver.Sink. Buffers the record's
// bytes; flushes when the buffer reaches BatchBytes.
func (s *Sink) OnRecord(ctx context.Context, rec logicalreceiver.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.records == 0 {
		s.startLSN = rec.WALStart
	}
	s.endLSN = rec.WALStart + pglogrepl.LSN(len(rec.Data))
	s.buf.Write(rec.Data)
	s.records++

	if s.buf.Len() >= s.opts.BatchBytes {
		return s.flushLocked(ctx)
	}
	return nil
}

// Tick checks BatchInterval and flushes if the interval has elapsed
// since the last flush AND there's anything to flush. Callers (the
// receive loop's ticker goroutine) call this periodically; it's
// cheap when there's nothing to do.
func (s *Sink) Tick(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == 0 {
		return nil
	}
	last := time.Unix(0, s.lastFlush.Load())
	if time.Since(last) < s.opts.BatchInterval {
		return nil
	}
	return s.flushLocked(ctx)
}

// Flush forces a flush of the in-flight batch (or no-ops if empty).
// Useful before clean shutdown.
func (s *Sink) Flush(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == 0 {
		return nil
	}
	return s.flushLocked(ctx)
}

// SyncedLSN implements logicalreceiver.Sink.
func (s *Sink) SyncedLSN() pglogrepl.LSN { return pglogrepl.LSN(s.syncedLSN.Load()) }

// flushLocked chunks the buffered bytes through the CAS, commits a
// SegmentManifest, advances syncedLSN. mu must be held.
func (s *Sink) flushLocked(ctx context.Context) error {
	if s.records == 0 {
		return nil
	}
	body := s.buf.Bytes()
	ch := s.opts.ChunkerFactory()
	var refs []ChunkRef
	for c, err := range ch.Iter(bytes.NewReader(body)) {
		if err != nil {
			return fmt.Errorf("chunked: chunker: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		bs := append([]byte(nil), c.Data...)
		info, perr := s.cas.PutChunk(ctx, bs)
		if perr != nil {
			return fmt.Errorf("chunked: cas put: %w", perr)
		}
		refs = append(refs, ChunkRef{
			Hash:   info.Hash,
			Offset: c.Offset,
			Len:    info.Size,
		})
	}

	m := &SegmentManifest{
		Schema:     Schema,
		Deployment: s.opts.Deployment,
		StreamName: s.opts.StreamName,
		Slot:       s.opts.Slot,
		Plugin:     s.opts.Plugin,
		StartLSN:   s.startLSN.String(),
		EndLSN:     s.endLSN.String(),
		Records:    s.records,
		BytesIn:    int64(len(body)),
		Chunks:     refs,
		CreatedAt:  time.Now().UTC(),
	}
	if err := s.commitManifest(ctx, m); err != nil {
		return err
	}

	s.syncedLSN.Store(uint64(s.endLSN))
	s.lastFlush.Store(time.Now().UnixNano())

	// Reset state for the next batch.
	s.buf.Reset()
	s.records = 0
	s.startLSN = 0
	s.endLSN = 0
	return nil
}

// commitManifest writes the manifest body to <key>.tmp and atomically
// renames to <key>. Idempotent on rename collision (a re-flush of the
// same StartLSN succeeds because the chunks are content-addressed).
func (s *Sink) commitManifest(ctx context.Context, m *SegmentManifest) error {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	key := SegmentPath(m.Deployment, m.StreamName, s.startLSN)
	tmp := key + ".tmp"
	if _, err := s.sp.Put(ctx, tmp, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	}); err != nil {
		return fmt.Errorf("chunked: put tmp manifest: %w", err)
	}
	if err := s.sp.RenameIfNotExists(ctx, tmp, key); err != nil {
		_ = s.sp.Delete(ctx, tmp)
		if errors.Is(err, storage.ErrAlreadyExists) {
			// Idempotent: a previous flush at this start_lsn
			// already committed this batch. Chunks deduplicated
			// naturally; the existing manifest is correct.
			return nil
		}
		return fmt.Errorf("chunked: commit manifest: %w", err)
	}
	return nil
}

// SegmentPath is the canonical repo key for a logical-stream batch.
// The `<start-lsn>` filename embeds the batch's first record LSN so
// the lex-greatest filename in the directory == the most recent
// committed batch (LSNs sort lexicographically the same as
// numerically when zero-padded). We DON'T zero-pad in v0.1 — the LSN
// string ("0/3F5A1B40") sorts adequately for the segment counts a
// single deployment produces.
func SegmentPath(deployment, streamName string, startLSN pglogrepl.LSN) string {
	return fmt.Sprintf("logical/%s/%s/%s.json",
		deployment, streamName, replaceSlash(startLSN.String()))
}

// replaceSlash converts "0/3F5A1B40" to "0_3F5A1B40" for filesystem
// safety. Slashes in S3 keys are fine (they're prefix delimiters)
// but the local fs backend would treat them as path separators and
// scatter our segments across pseudo-directories. The reverse
// operation isn't needed — readers split by underscore.
func replaceSlash(s string) string { return strings.ReplaceAll(s, "/", "_") }
