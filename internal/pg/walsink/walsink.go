// Package walsink implements replication.WALSink: it assembles the
// byte stream of XLogData messages into 16 MiB PostgreSQL WAL segments,
// runs each completed segment through the chunker into the CAS, and
// commits a per-segment manifest atomically.
//
// Architecture — a two-stage pipeline:
//
//	replication.Stream  --OnRecord(XLogRecord)-->  receive side
//	                                                    |
//	                                          fill 16 MiB buffer
//	                                                    |
//	                                       hand off via `pending` chan
//	                                                    |
//	                                        ============================
//	                                        background processor goroutine
//	                                        ----------------------------
//	                                        chunk segment (worker pool)
//	                                        adaptively batch N segments
//	                                        ONE cas.Barrier per batch
//	                                        commit manifests in order
//	                                        SyncedLSN.Store(endLSN)
//
// Why the pipeline split? OnRecord runs on replication.Stream's single
// receive goroutine and its interface contract says it "must return
// promptly". An earlier design ran the whole chunk→barrier→commit
// inline in OnRecord, so WAL receipt STALLED for the entire processing
// of every 16 MiB segment — receive and process never overlapped, and
// a VACUUM FULL burst (~100 MB/s of WAL) left the streamer ~70 GiB
// behind. Now OnRecord only memcpy's into a buffer and hands a filled
// segment to the processor; the receive goroutine returns to the
// socket immediately. Receipt and processing run concurrently.
//
// Why batch the Barrier? cas.Barrier is one syncfs(2) — it flushes the
// whole filesystem. Issuing it per 16 MiB segment, on a disk a busy
// primary is also writing to, gates the streamer behind the primary's
// total write rate. The processor instead accumulates up to batchCap
// segments and issues ONE Barrier for the batch (adaptive: under a
// trickle the batch is a single segment, so SyncedLSN still advances
// promptly; under a burst it grows and the syncfs cost is amortised).
//
// Why per-segment commits and not finer-grained?
//
//   - PG's WAL replay operates on segment-aligned files. A 4 MiB partial
//     would require us to invent a partial-replay protocol — far easier
//     to wait for the segment to fill and commit it whole.
//
//   - SyncedLSN is read by the replication-loop's status ticker to tell
//     PG how far the standby has flushed. PG advances the slot's
//     restart_lsn no further than this value, so a crash between
//     segment commits causes PG to resend bytes from the start of the
//     partially-filled segment on restart. Idempotent and gap-free.
//
// Core invariant: SyncedLSN is advanced past a segment ONLY after that
// segment's chunks have been Barrier'd crash-durable AND its manifest
// committed. The processor commits a batch's manifests in strictly
// ascending segment order, so the highest committed manifest in the
// repo is always contiguous — a crash mid-batch resumes cleanly with no
// WAL hole.
//
// Resilience contract (failure-mode by failure-mode):
//
//   - Out-of-order WAL bytes (offset != bufFilled): error from OnRecord.
//     The replication slot makes this impossible under normal
//     operation; surfacing it loudly catches bugs in upstream layers.
//
//   - Mid-stream segment skip without filling the previous: error
//     (gap detection) from OnRecord.
//
//   - chunker / cas.PutChunk / barrier / commit error: recorded on the
//     Sink; the next OnRecord and Flush/Close return it; SyncedLSN is
//     left at the last cleanly-committed segment; on retry PG resends
//     from there.
//
//   - manifest commit conflict (RenameIfNotExists -> ErrAlreadyExists):
//     idempotent success. Prior agent ran already committed this exact
//     segment; chunks deduplicated naturally; we leave the existing
//     manifest in place rather than overwriting.
package walsink

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pglogrepl"
	"golang.org/x/sync/errgroup"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/chunker"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/obs/metrics"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// SegmentSize is PG's DEFAULT WAL segment size: 16 MiB, and the value
// used throughout when a caller doesn't specify one. PG can be
// initialised with non-default sizes via initdb --wal-segsize (a power
// of two, 1 MiB to 1 GiB, PG 11+); the streamer probes the cluster's
// actual size and threads it through, defaulting to this when unknown.
const SegmentSize = 16 * 1024 * 1024

// DefaultSegmentSize is the typed form of SegmentSize, used where the
// segment size is a configurable int64 resolved from 0 → default.
const DefaultSegmentSize int64 = SegmentSize

// MinSegmentSize and MaxSegmentSize bound a valid initdb --wal-segsize:
// 1 MiB to 1 GiB. A WAL segment size outside this range (or not a power
// of two) is something PG cannot produce.
const (
	MinSegmentSize int64 = 1 << 20 // 1 MiB
	MaxSegmentSize int64 = 1 << 30 // 1 GiB
)

// ValidSegmentSize reports whether s is a WAL segment size a PG cluster
// can be initialised with: a power of two in [1 MiB, 1 GiB].
func ValidSegmentSize(s int64) bool {
	return s >= MinSegmentSize && s <= MaxSegmentSize && (s&(s-1)) == 0
}

// NormSegmentSize resolves 0 (or any non-positive value) to the default
// 16 MiB, and otherwise returns s unchanged. Callers at the edge of the
// package use this so "unset" means "the historical default".
func NormSegmentSize(s int64) int64 {
	if s <= 0 {
		return DefaultSegmentSize
	}
	return s
}

// pipelineDepthFor bounds the segment-buffer pool so its upfront
// allocation stays near pipelineBufferBudget (256 MiB) regardless of
// segment size, while keeping at least 2 buffers so the receive side can
// pipeline one segment ahead of the processor: depth = clamp(budget /
// segSize, 2, 16). 16 MiB → 16; 64 MiB → 4; 256 MiB → 2; 1 GiB → 2
// (a 2-buffer floor means a 1 GiB cluster's pool is 2 GiB — the minimum
// for any overlap at that size).
func pipelineDepthFor(segSize int64) int {
	d := int(int64(pipelineBufferBudget) / NormSegmentSize(segSize))
	if d < 2 {
		return 2
	}
	if d > pipelineDepth {
		return pipelineDepth
	}
	return d
}

// SegmentsPerLog returns how many WAL segments PG packs into a single
// 4 GiB log-id grouping (the middle 8 hex chars of a segment name) for
// the given segment size: 0x1_0000_0000 / s. With the default 16 MiB
// that's 256; with 64 MiB it's 64; with 1 MiB it's 4096. The segment
// size must be valid (a power-of-two divisor of 4 GiB); callers pass a
// value resolved through NormSegmentSize.
func SegmentsPerLog(segmentSize int64) uint64 {
	return uint64(0x100000000) / uint64(NormSegmentSize(segmentSize))
}

// Pipeline tunables. These bound the streamer's in-flight memory and
// the syncfs amortisation factor.
const (
	// pipelineDepth is the MAXIMUM `pending` channel capacity and
	// segment-buffer pool size, used for the default 16 MiB segment —
	// so at most pipelineDepth 16 MiB buffers (256 MiB) are ever live.
	// For larger wal_segment_size values the depth is scaled DOWN by
	// pipelineDepthFor so the upfront buffer-pool allocation stays
	// bounded (a fixed depth of 16 would allocate 16 GiB for 1 GiB
	// segments). OnRecord blocks when the pool is drained: correct
	// backpressure if the processor falls behind.
	pipelineDepth = 16

	// pipelineBufferBudget caps the total bytes the segment-buffer pool
	// may pre-allocate, across all sizes: depth = clamp(budget/seg, 2, 16).
	pipelineBufferBudget = 256 << 20 // 256 MiB

	// batchCap is the most segments the processor folds under a single
	// cas.Barrier. Larger amortises syncfs further but delays SyncedLSN
	// under a sustained burst; 16 segments = 256 MiB of WAL.
	batchCap = 16
)

// Sink implements replication.WALSink.
//
// Goroutine model: OnRecord runs on replication.Stream's single receive
// goroutine and must not be called concurrently. A background processor
// goroutine (started by New) does all chunking, the Barrier and the
// manifest commits. SyncedLSN is read concurrently by the replication
// loop's status ticker — hence the atomic. Flush and Close coordinate
// with the processor over channels. Close must be called exactly once,
// after the last OnRecord, to drain the pipeline and stop the goroutine.
type Sink struct {
	cas              *repo.CAS
	sp               storage.StoragePlugin
	opts             Options
	segSize          int64 // resolved wal_segment_size (opts.SegmentSize, 0 → default)
	chunkerFn        func() *chunker.Chunker
	chunkConcurrency int

	// Receive-side per-segment buffer + state. curBuf is borrowed from
	// bufPool; on a full segment it is handed to the processor and a
	// fresh one is borrowed. The partial final segment is never handed
	// off (PG resends it on the next run).
	curBuf      []byte
	curFilled   int
	curSegNum   uint64
	curStartLSN pglogrepl.LSN
	haveSeg     bool

	// Pipeline plumbing.
	bufPool  chan []byte   // free 16 MiB segment buffers
	pending  chan *segJob  // filled segments handed receive -> processor
	procDone chan struct{} // closed when the processor goroutine exits
	procCtx  context.Context
	procStop context.CancelFunc // hard-aborts the processor (Close timeout)

	closeOnce sync.Once

	// procErr holds the first processing error. OnRecord and Flush/Close
	// surface it; first writer wins. nil until something fails.
	procErr atomic.Pointer[error]

	// syncedLSN is the end LSN of the most recently committed segment.
	// SyncedLSN reads it; the processor writes it. Atomic because the
	// replication loop's status ticker runs on a separate goroutine.
	syncedLSN atomic.Uint64

	// bufferedLSN is the highest end-of-record LSN OnRecord has seen
	// from the upstream — INCLUDING bytes that are buffered into the
	// current partial segment but not yet committed.  It is strictly
	// monotonic and is updated by the receive goroutine only.
	// BufferedLSN reads it for diagnostic / reporting purposes; PG's
	// slot is NEVER advanced past syncedLSN, so a partial segment's
	// bytes are guaranteed to be resent on reconnect.
	bufferedLSN atomic.Uint64
}

// segJob is one unit handed from the receive side to the processor.
// A job with flush != nil is a Flush sentinel, not a real segment.
type segJob struct {
	buf      []byte
	n        int
	segNum   uint64
	startLSN pglogrepl.LSN
	flush    chan error
}

// chunkedSeg is a segment after chunking, awaiting the batch Barrier
// and its manifest commit. It no longer references the 16 MiB buffer
// (returned to the pool once chunked).
type chunkedSeg struct {
	segNum   uint64
	startLSN pglogrepl.LSN
	refs     []ChunkRef
}

// DurabilityMode selects how the streamer makes a finished
// segment's chunks crash-durable before its manifest is committed.
type DurabilityMode string

const (
	// DurabilityPerSegment writes chunks DurabilityDeferred and issues
	// one cas.Barrier per processor BATCH before that batch's manifests
	// commit. Under a trickle a batch is one segment (~1 syncfs per
	// 16 MiB); under a burst the batch grows and the syncfs is
	// amortised. The default.
	DurabilityPerSegment DurabilityMode = "per-segment"

	// DurabilityPerChunk fsyncs every chunk inline: the streamer's
	// CAS is built DurabilityInline and the processor skips the
	// Barrier. Slower — the "fsync every object" compliance opt-in.
	DurabilityPerChunk DurabilityMode = "per-chunk"
)

// Options configures a Sink.
type Options struct {
	// Deployment is the logical deployment name. Becomes part of the
	// repo path (`wal/<deployment>/...`) and is recorded on every
	// segment manifest.
	Deployment string

	// Timeline is the PG timeline ID the stream is on. The agent
	// captures it from IDENTIFY_SYSTEM at stream start. v0.1 expects
	// a fixed timeline for the lifetime of the Sink; mid-stream
	// timeline switches (rare, occur only on PG promotion) require
	// resetting the Sink with the new TLI.
	Timeline uint32

	// SystemIdentifier is the cluster's pg_control system_identifier
	// (also from IDENTIFY_SYSTEM). Stamped on every manifest so
	// cross-cluster contamination of a repo is detectable at restore
	// time.
	SystemIdentifier string

	// SegmentSize is the cluster's wal_segment_size in bytes. The
	// streamer chops WAL at this boundary, names segments using it
	// (PG packs 4 GiB / SegmentSize segments per log-id), and records
	// it on every manifest. 0 resolves to the default 16 MiB; the
	// caller probes the cluster's actual value and passes it here.
	SegmentSize int64

	// ChunkerFactory builds a fresh chunker per segment. Defaults to
	// chunker.New (4 KiB / 64 KiB / 256 KiB FastCDC). Tests override
	// to exercise tighter or looser bounds.
	ChunkerFactory func() *chunker.Chunker

	// WORM, when non-nil, propagates a retention deadline to every
	// per-segment manifest Put. The deadline is computed at commit
	// time so each segment's retention starts when it archives, not
	// when the Sink was constructed. Backends without Object-Lock
	// support (fs) silently ignore the field.
	//
	// Chunk retention is the caller's concern: pass a CAS built via
	// casdefault.NewWithRetention(sp, worm, now) when chunks should
	// also be locked.
	WORM *repo.WORMPolicy

	// Encryption, when non-nil, is recorded on every segment manifest the
	// streamer commits so restore can resolve the shared DEK from the
	// segment alone. The caller MUST build the streamer's CAS with the
	// matching encrypting constructor (casdefault.NewEncrypted*) and the
	// same DEK; this field only stamps the envelope (issue #106).
	Encryption *EncryptionInfo

	// Durability selects how a segment's chunks are made
	// crash-durable. Empty defaults to DurabilityPerSegment.
	//
	// The streamer's CAS must be built to MATCH this: per-segment
	// needs a DurabilityDeferred CAS (casdefault.WithChunkDurability),
	// per-chunk needs an Inline one. wal.go resolves the --durability
	// flag and wires both together.
	Durability DurabilityMode

	// ChunkConcurrency bounds how many of a segment's chunks are
	// compressed/encrypted/written in parallel by the processor.
	// 0 → min(GOMAXPROCS, 8); 1 → the legacy serial path.
	ChunkConcurrency int

	// FaultHook, when non-nil, fires at named checkpoints in the
	// processor's chunk → commit → LSN-advance pipeline. Returning a
	// non-nil error aborts processing as if the agent had crashed
	// there; the test harness then verifies that resuming the stream
	// produces a byte-equal final manifest set.
	//
	// Checkpoints fired (per segment, in finalize order):
	//
	//   - "after_chunk_uploaded"   — after each cas.PutChunk succeeds.
	//   - "before_manifest_commit" — chunks durable, manifest unwritten.
	//   - "after_manifest_rename"  — manifest at its canonical key.
	//   - "before_lsn_advance"     — immediately before SyncedLSN moves.
	//
	// nil → no checkpoints fire, zero overhead.
	FaultHook FaultHook
}

// FaultHook is the per-checkpoint callback Options.FaultHook uses.
// Returning a non-nil error aborts the in-flight operation; the test
// harness treats the abort as a simulated agent crash and verifies the
// resume invariant.
type FaultHook func(ctx context.Context, checkpoint string) error

// Named checkpoints the Sink fires its FaultHook at. Stable across
// releases — used by tests as string constants.
const (
	HookAfterChunkUploaded   = "after_chunk_uploaded"
	HookBeforeManifestCommit = "before_manifest_commit"
	HookAfterManifestRename  = "after_manifest_rename"
	HookBeforeLSNAdvance     = "before_lsn_advance"
)

// New returns a Sink ready to receive XLogRecords and starts its
// background processor goroutine. cas and sp are retained but not
// closed; the caller manages their lifecycle. The caller MUST call
// Close exactly once when streaming ends, to drain the pipeline and
// stop the processor.
func New(cas *repo.CAS, sp storage.StoragePlugin, opts Options) (*Sink, error) {
	if cas == nil {
		return nil, errors.New("walsink: nil CAS")
	}
	if sp == nil {
		return nil, errors.New("walsink: nil StoragePlugin")
	}
	if opts.Deployment == "" {
		return nil, errors.New("walsink: empty deployment")
	}
	if opts.SystemIdentifier == "" {
		return nil, errors.New("walsink: empty SystemIdentifier")
	}
	segSize := NormSegmentSize(opts.SegmentSize)
	if !ValidSegmentSize(segSize) {
		return nil, fmt.Errorf("walsink: invalid wal_segment_size %d (want a power of two in [1 MiB, 1 GiB])", opts.SegmentSize)
	}
	depth := pipelineDepthFor(segSize)
	procCtx, procStop := context.WithCancel(context.Background())
	s := &Sink{
		cas:              cas,
		sp:               sp,
		opts:             opts,
		segSize:          segSize,
		chunkerFn:        opts.ChunkerFactory,
		chunkConcurrency: resolveChunkConcurrency(opts.ChunkConcurrency),
		bufPool:          make(chan []byte, depth),
		pending:          make(chan *segJob, depth),
		procDone:         make(chan struct{}),
		procCtx:          procCtx,
		procStop:         procStop,
	}
	if s.chunkerFn == nil {
		s.chunkerFn = chunker.New
	}
	for i := 0; i < depth; i++ {
		s.bufPool <- make([]byte, segSize)
	}
	go s.run()
	return s, nil
}

// resolveChunkConcurrency mirrors tarsink: 0 → min(GOMAXPROCS, 8),
// negative/1 → serial, capped at 8 (diminishing returns past that for
// the compress+encrypt+Put mix, and a streamer should not monopolise a
// many-core host).
func resolveChunkConcurrency(n int) int {
	if n > 0 {
		if n > 8 {
			return 8
		}
		return n
	}
	g := runtime.GOMAXPROCS(0)
	if g > 8 {
		g = 8
	}
	if g < 1 {
		g = 1
	}
	return g
}

// OnRecord implements replication.WALSink. It buffers WAL bytes into
// the current segment and, on a 16 MiB boundary, hands the filled
// segment to the background processor — returning promptly so the
// receive loop can keep draining the socket. A single XLogRecord may
// span multiple segments.
func (s *Sink) OnRecord(ctx context.Context, rec replication.XLogRecord) error {
	if err := s.procErrLoad(); err != nil {
		return err
	}
	// A cancelled ctx aborts before any buffering or hand-off — so a
	// dead stream context deterministically stops receipt rather than
	// racing the buffer-pool / pending-channel selects below.
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(rec.Data) == 0 {
		return nil
	}
	pos := uint64(rec.WALStart)
	data := rec.Data

	for len(data) > 0 {
		segSize := uint64(s.segSize)
		segNum := pos / segSize
		offsetInSeg := pos % segSize

		if !s.haveSeg {
			if err := s.startSegment(ctx, segNum, pglogrepl.LSN(segNum*segSize)); err != nil {
				return err
			}
		}

		// Mismatched segment without a clean finalize means the
		// previous segment is partial and we just received bytes for
		// the next — that's a gap, the slot's existence makes it
		// indistinguishable from upstream corruption, refuse loudly.
		if segNum != s.curSegNum {
			return fmt.Errorf("walsink: gap detected: was filling segment %d (%d/%d bytes); next bytes target segment %d",
				s.curSegNum, s.curFilled, s.segSize, segNum)
		}

		// Strict in-order: the offset within the segment must equal
		// what we've already buffered. Replication via the slot
		// guarantees this; surfacing a mismatch catches upstream
		// regressions.
		if offsetInSeg != uint64(s.curFilled) {
			return fmt.Errorf("walsink: out-of-order WAL: segment %d expected offset %d got %d",
				segNum, s.curFilled, offsetInSeg)
		}

		room := int(s.segSize) - s.curFilled
		n := len(data)
		if n > room {
			n = room
		}
		copy(s.curBuf[s.curFilled:], data[:n])
		s.curFilled += n
		pos += uint64(n)
		data = data[n:]

		// Publish the new receive frontier for diagnostic readers
		// (BufferedLSN).  Monotonic because the loop only advances pos.
		s.bufferedLSN.Store(pos)

		if s.curFilled == int(s.segSize) {
			if err := s.handOff(ctx); err != nil {
				return err
			}
			// Loop continues: any remaining data goes into the next
			// segment via the not-haveSeg path.
		}
	}
	return nil
}

// startSegment borrows a fresh buffer from the pool and initialises
// per-segment receive state. Blocks (backpressure) if the pool is
// drained — i.e. the processor is pipelineDepth segments behind.
func (s *Sink) startSegment(ctx context.Context, segNum uint64, startLSN pglogrepl.LSN) error {
	select {
	case buf := <-s.bufPool:
		s.curBuf = buf
	case <-ctx.Done():
		return ctx.Err()
	}
	s.curSegNum = segNum
	s.curStartLSN = startLSN
	s.curFilled = 0
	s.haveSeg = true
	return nil
}

// handOff sends the filled current segment to the processor.
func (s *Sink) handOff(ctx context.Context) error {
	job := &segJob{
		buf:      s.curBuf,
		n:        s.curFilled,
		segNum:   s.curSegNum,
		startLSN: s.curStartLSN,
	}
	select {
	case s.pending <- job:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.curBuf = nil
	s.haveSeg = false
	s.curFilled = 0
	return nil
}

// SyncedLSN implements replication.WALSink. Returns the end LSN of the
// most recently committed segment (segment-aligned). The replication
// loop's status ticker forwards this to PG.
func (s *Sink) SyncedLSN() pglogrepl.LSN {
	return pglogrepl.LSN(s.syncedLSN.Load())
}

// BufferedLSN returns the highest end-of-record LSN OnRecord has
// observed from the upstream — INCLUDING the bytes accumulating in
// the current partial 16 MiB segment.  It is strictly monotonic and
// is intended for diagnostic reporting (the wal-stream stop summary,
// the progress ticker), NOT for any durability decision.
//
// Why expose this?  Operators stopping the stream mid-segment see no
// `SyncedLSN` progress because no segment has committed; BufferedLSN
// shows that bytes WERE received, just not yet durable in the repo.
// PG will resend them on reconnect (the slot's restart_lsn is still
// at the last committed boundary), so the on-disk state is
// gap-free.
//
// BufferedLSN >= SyncedLSN at all times after the first OnRecord.
// Before the first record they are both zero.
func (s *Sink) BufferedLSN() pglogrepl.LSN {
	return pglogrepl.LSN(s.bufferedLSN.Load())
}

// SegmentSize returns the resolved wal_segment_size (bytes) this Sink
// chops and names segments with. Callers (e.g. progress reporting) use
// it to derive segment names from an LSN.
func (s *Sink) SegmentSize() int64 {
	return s.segSize
}

// Flush blocks until every segment handed off so far has been chunked,
// Barrier'd and committed (so SyncedLSN reflects them), then returns
// any processing error. It does NOT stop the processor — streaming can
// continue afterwards. The partially-filled current segment is not
// affected (it has not been handed off).
func (s *Sink) Flush(ctx context.Context) error {
	if err := s.procErrLoad(); err != nil {
		return err
	}
	reply := make(chan error, 1)
	select {
	case s.pending <- &segJob{flush: reply}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close drains the pipeline — committing every handed-off segment —
// stops the processor goroutine and returns the first processing
// error, if any. It must be called exactly once, after the last
// OnRecord. If ctx is cancelled while draining, the processor is
// hard-aborted (in-flight CAS work is cancelled) and Close returns
// promptly; SyncedLSN then reflects only the segments committed before
// the abort.
func (s *Sink) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		close(s.pending)
	})
	select {
	case <-s.procDone:
	case <-ctx.Done():
		s.procStop()
		<-s.procDone
	}
	s.procStop() // release the context regardless
	return s.procErrLoad()
}

// run is the background processor: it receives filled segments,
// adaptively batches them, issues one Barrier per batch and commits
// each batch's manifests in ascending segment order.
func (s *Sink) run() {
	defer close(s.procDone)
	var batch []chunkedSeg
	failed := false

	for job := range s.pending {
		if failed {
			// Pipeline already broken — keep draining so a producer
			// blocked on `pending` (OnRecord) or `reply` (Flush) is
			// released rather than dead-locked.
			if job.flush != nil {
				job.flush <- s.procErrLoad()
			} else {
				s.bufPool <- job.buf
			}
			continue
		}

		if job.flush != nil {
			err := s.flushBatch(batch)
			batch = batch[:0]
			if err != nil {
				failed = true
				s.procErrStore(err)
			}
			job.flush <- err
			continue
		}

		cs, err := s.chunkSegment(job)
		if err != nil {
			failed = true
			s.procErrStore(err)
			continue
		}
		batch = append(batch, cs)

		// Adaptive flush: when nothing else is queued, commit now so
		// SyncedLSN advances promptly; under a sustained burst the
		// batch grows to batchCap and one Barrier covers it all.
		if len(batch) >= batchCap || len(s.pending) == 0 {
			if err := s.flushBatch(batch); err != nil {
				failed = true
				s.procErrStore(err)
			}
			batch = batch[:0]
		}
	}

	// pending closed by Close: commit whatever is still batched.
	if !failed {
		if err := s.flushBatch(batch); err != nil {
			s.procErrStore(err)
		}
	}
}

// chunkSegment runs one filled segment through the chunker into the
// CAS, fanning the per-chunk compress+encrypt+Put across a worker pool.
// The 16 MiB buffer is returned to the pool as soon as the chunker has
// copied every chunk's bytes out of it.
func (s *Sink) chunkSegment(job *segJob) (chunkedSeg, error) {
	if err := s.procCtx.Err(); err != nil {
		// Return the borrowed segment buffer to the pool before bailing —
		// every other exit from chunkSegment does (the chunker-error path
		// below and the success path), and leaving it out here leaks a
		// 16 MiB pooled buffer (resource-cleanup audit #1). Benign today
		// because procCtx is only cancelled at Close, but consistent and
		// correct on every path.
		s.bufPool <- job.buf
		return chunkedSeg{}, err
	}
	ch := s.chunkerFn()
	type item struct {
		off  int64
		data []byte
	}
	var items []item
	for c, err := range ch.Iter(bytes.NewReader(job.buf[:job.n])) {
		if err != nil {
			s.bufPool <- job.buf
			return chunkedSeg{}, fmt.Errorf("walsink: chunker: %w", err)
		}
		// The chunker reuses its buffer across iterations; copy before
		// the next yield.
		items = append(items, item{off: c.Offset, data: append([]byte(nil), c.Data...)})
	}
	// Every chunk's bytes are copied out — the buffer is free for the
	// receive side to refill while we compress/encrypt/upload.
	s.bufPool <- job.buf

	refs := make([]ChunkRef, len(items))
	g, gctx := errgroup.WithContext(s.procCtx)
	g.SetLimit(s.chunkConcurrency)
	for i := range items {
		i := i
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err
			}
			info, err := s.cas.PutChunk(gctx, items[i].data)
			if err != nil {
				return fmt.Errorf("walsink: cas put: %w", err)
			}
			refs[i] = ChunkRef{Hash: info.Hash, Offset: items[i].off, Len: info.Size}
			// Fault checkpoint: a chunk landed in CAS, manifest not yet
			// written. Fires concurrently across the pool — a hook that
			// errors aborts the whole segment via errgroup.
			if hook := s.opts.FaultHook; hook != nil {
				if err := hook(gctx, HookAfterChunkUploaded); err != nil {
					return fmt.Errorf("walsink: fault@%s: %w", HookAfterChunkUploaded, err)
				}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return chunkedSeg{}, err
	}
	return chunkedSeg{segNum: job.segNum, startLSN: job.startLSN, refs: refs}, nil
}

// flushBatch makes a batch of chunked segments durable: one Barrier
// for all of them, then a manifest commit per segment in ascending
// order, advancing SyncedLSN as each commits. The Barrier MUST precede
// every commit — a manifest is never committed before the chunks it
// references are crash-durable.
func (s *Sink) flushBatch(batch []chunkedSeg) error {
	if len(batch) == 0 {
		return nil
	}
	if err := s.procCtx.Err(); err != nil {
		return err
	}

	// Durability barrier: in per-segment mode every chunk of every
	// segment in the batch was written DurabilityDeferred (no per-chunk
	// fsync); one Barrier makes them all crash-durable before any
	// manifest — which references them, and whose commit advances
	// SyncedLSN — is written. On an InlineDurable CAS (object stores)
	// Barrier is a cheap no-op. In per-chunk mode the CAS is
	// DurabilityInline, every chunk already fsync'd, so it is skipped.
	if s.opts.Durability != DurabilityPerChunk {
		if err := s.cas.Barrier(s.procCtx); err != nil {
			return fmt.Errorf("walsink: durability barrier: %w", err)
		}
	}

	for _, cs := range batch {
		endLSN := cs.startLSN + pglogrepl.LSN(s.segSize)
		m := &SegmentManifest{
			Schema:           Schema,
			Deployment:       s.opts.Deployment,
			SystemIdentifier: s.opts.SystemIdentifier,
			Timeline:         s.opts.Timeline,
			SegmentNumber:    cs.segNum,
			SegmentName:      SegmentFileName(s.opts.Timeline, cs.segNum, s.segSize),
			StartLSN:         cs.startLSN.String(),
			EndLSN:           endLSN.String(),
			SegmentSize:      s.segSize,
			Chunks:           cs.refs,
			CreatedAt:        time.Now().UTC(),
			Encryption:       s.opts.Encryption,
		}

		// Fault checkpoint: chunks durable, manifest built, not committed.
		if hook := s.opts.FaultHook; hook != nil {
			if err := hook(s.procCtx, HookBeforeManifestCommit); err != nil {
				return fmt.Errorf("walsink: fault@%s: %w", HookBeforeManifestCommit, err)
			}
		}

		if err := s.commitManifest(s.procCtx, m); err != nil {
			return err
		}

		// Fault checkpoint: manifest at its canonical key, segment fully
		// durable; SyncedLSN not yet advanced (PG will resend on resume).
		if hook := s.opts.FaultHook; hook != nil {
			if err := hook(s.procCtx, HookAfterManifestRename); err != nil {
				return fmt.Errorf("walsink: fault@%s: %w", HookAfterManifestRename, err)
			}
		}
		// Fault checkpoint: distinct hook immediately before the LSN
		// store, so a future reorder cannot silently break the contract.
		if hook := s.opts.FaultHook; hook != nil {
			if err := hook(s.procCtx, HookBeforeLSNAdvance); err != nil {
				return fmt.Errorf("walsink: fault@%s: %w", HookBeforeLSNAdvance, err)
			}
		}

		s.syncedLSN.Store(uint64(endLSN))

		// One segment durably archived via the streaming path. The push
		// (archive_command) path emits the same counter; without this a
		// streaming-only deployment — pg_hardstorage's primary WAL mode —
		// reported zero WAL archival in metrics. segSize is the logical
		// segment width (dedup/compression shrink the on-disk footprint).
		metrics.WALSegmentArchived(s.opts.Deployment, s.segSize)
	}
	return nil
}

// procErrStore records the first processing error (first writer wins).
func (s *Sink) procErrStore(err error) {
	if err == nil {
		return
	}
	s.procErr.CompareAndSwap(nil, &err)
}

// procErrLoad returns the recorded processing error, or nil.
func (s *Sink) procErrLoad() error {
	if p := s.procErr.Load(); p != nil {
		return *p
	}
	return nil
}

// commitManifest writes the manifest body to <key>.tmp and atomically
// renames it to the canonical key. RenameIfNotExists makes a re-commit
// of an existing segment a no-op (idempotent).
func (s *Sink) commitManifest(ctx context.Context, m *SegmentManifest) error {
	lock, err := repo.AcquireMutationLock(ctx, s.sp, "WAL manifest "+m.SegmentName)
	if err != nil {
		return fmt.Errorf("walsink: commit mutation lock: %w", err)
	}
	defer func() { _ = lock.Release(context.Background()) }()
	if err := ensureSegmentChunksPresent(ctx, s.sp, m); err != nil {
		return err
	}
	body, err := m.MarshalToBytes()
	if err != nil {
		return err
	}
	key := SegmentPath(m.Deployment, m.Timeline, m.SegmentName)
	tmp := key + ".tmp." + randSuffix()

	putOpts := storage.PutOptions{ContentLength: int64(len(body))}
	if !s.opts.WORM.IsZero() {
		now := time.Now().UTC()
		putOpts.RetainUntil = s.opts.WORM.RetainUntil(now)
		putOpts.RetentionMode = storage.WORMMode(s.opts.WORM.Mode)
	}
	if _, err := s.sp.Put(ctx, tmp, bytes.NewReader(body), putOpts); err != nil {
		return fmt.Errorf("walsink: put tmp manifest: %w", err)
	}
	if err := s.sp.RenameIfNotExists(ctx, tmp, key); err != nil {
		// Best-effort tmp cleanup; never propagate a tmp-cleanup
		// failure (the rename's failure is what the caller cares about).
		_ = s.sp.Delete(ctx, tmp)
		if errors.Is(err, storage.ErrAlreadyExists) {
			// Existing manifest at this key: either a true idempotent
			// re-commit (prior agent run already got here, chunks
			// dedup'd, manifests match) OR a split-brain doppelgänger
			// (two clusters with cloned system_identifier streaming
			// into the same repo). verifyExistingManifest reads the
			// on-disk manifest and returns nil only when every
			// identifying field — sysid + ordered chunk-list —
			// matches; otherwise a splitbrain.* structured error.
			return verifyExistingManifest(ctx, s.sp, key, m)
		}
		return fmt.Errorf("walsink: rename manifest: %w", err)
	}
	return nil
}

func ensureSegmentChunksPresent(ctx context.Context, sp storage.StoragePlugin, m *SegmentManifest) error {
	for _, ref := range m.Chunks {
		if _, err := sp.Stat(ctx, repo.ChunkKey(ref.Hash)); err != nil {
			return fmt.Errorf("walsink: refusing to commit segment %s: referenced chunk %s is unavailable after repository mutation fencing: %w", m.SegmentName, ref.Hash, err)
		}
	}
	return nil
}

// randSuffix returns a short hex suffix for tmp-file names so
// concurrent commits (rare here, but possible if multiple agents ever
// stream the same slot) don't collide on the staging slot.
func randSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	const hex = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hex[c>>4]
		out[i*2+1] = hex[c&0xf]
	}
	return string(out)
}
