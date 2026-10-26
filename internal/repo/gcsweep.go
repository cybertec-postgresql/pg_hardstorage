// gcsweep.go — Sweep: the one engine behind `repo gc` and
// `repair chunks --orphans`: a single-walk reference snapshot, the
// orphan scan, and (with Apply) the fenced, batched delete.
package repo

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
)

// SweepOptions configures Sweep.
type SweepOptions struct {
	// TombstoneGrace and MinChunkAge follow CollectReferencesOptions /
	// FindOrphansOptions: zero = package default, negative = disabled.
	TombstoneGrace time.Duration
	MinChunkAge    time.Duration

	// Apply deletes; without it Sweep only reports.
	Apply bool

	// LiveLeases, when non-nil, returns the deployments holding a live
	// backup lease. Apply refuses to start while any exist and stops
	// deleting (ErrSweepBackupInFlight) when one appears mid-run —
	// defence in depth for writers that predate the commit fence.
	LiveLeases func(ctx context.Context) ([]string, error)

	// BatchSize is the number of chunks deleted between fence
	// checkpoints (default 4096). DeleteConcurrency bounds parallel
	// deletes (default 16).
	BatchSize         int
	DeleteConcurrency int

	// BatchDuration lets one fence checkpoint (run-record bump, pin read,
	// lease check and the manifest rescan) cover several BatchSize slices
	// of deletes for up to this long, so a repository with hundreds of
	// millions of orphans pays for the rescan once per BatchDuration of
	// deleting instead of once per BatchSize chunks. Zero means: adaptive
	// (GCFenceMaxWait/5) when BatchSize is also zero, else one checkpoint
	// per BatchSize slice. Negative forces one checkpoint per slice. It is
	// always capped so rescan + deletes stay well inside GCFenceMaxWait:
	// a writer waits for the whole checkpoint (see gcfence.go).
	BatchDuration time.Duration

	// Now is the clock (default time.Now).
	Now func() time.Time

	// RefSpill tunes the reference set's heap budget and spill
	// directory. The zero value keeps up to spill.DefaultMemEntries
	// hashes on the heap and spills the rest to the system temp dir.
	RefSpill spill.Options

	// OnWarning, when set, receives non-fatal findings (a pin or run
	// record that could not be cleaned up, ...).
	OnWarning func(msg string)

	// beforeBatch is a test hook run after a batch's fence checkpoint
	// has been published and BEFORE its pin read/rescan, so tests can
	// interleave writers deterministically.
	beforeBatch func(batch int)
	// beforeDelete is a test hook run after the checkpoint's pin read
	// and rescan, immediately before the batch's deletes.
	beforeDelete func(batch int)
}

// ErrSweepBackupInFlight is returned (wrapped) when a live backup lease
// blocks or stops an Apply sweep. Retry-safe.
var ErrSweepBackupInFlight = errors.New("repo: gc: a backup is in flight")

// SweepOrphanSampleLimit bounds SweepResult.Orphans: a 100 TB repository
// can hold hundreds of millions of orphans, and a report that names each
// one is itself an out-of-memory failure. The sample is the smallest
// hashes, in ascending order; OrphanCount is the true total.
const SweepOrphanSampleLimit = 1024

// maxSweepFailures bounds SweepResult.Failures (FailureCount is exact).
const maxSweepFailures = 1024

// sweepOrphanMemRecords is how many orphan records Sweep keeps on the heap
// before they go to a temp file (variable so tests can force a spill).
var sweepOrphanMemRecords = 1 << 20

// SweepResult is the outcome of Sweep.
type SweepResult struct {
	RefCount int
	// OrphanCount is the exact number of orphan chunks found; Orphans is
	// the smallest SweepOrphanSampleLimit of them, sorted.
	OrphanCount      int
	Orphans          []Hash // sorted sample (at most SweepOrphanSampleLimit)
	OrphanBytes      int64  // from the chunk listing; no per-orphan Stat
	StaleTemps       []string
	Deleted          int
	DeletedBytes     int64
	StaleTempDeleted int
	// StagingReaped / StagingReapedBytes count backend staging files
	// (hidden from List) left by crashed writers and removed by Apply.
	StagingReaped      int
	StagingReapedBytes int64
	// Failures holds the first maxSweepFailures per-key delete failures
	// (sorted); FailureCount is the exact number.
	Failures     []string
	FailureCount int
	// SkippedReferenced counts orphans spared because a manifest
	// committed after the snapshot, or a writer's pin, claimed them.
	SkippedReferenced int
	// StoppedEarly is non-empty when Apply stopped before the last batch
	// (live backup lease); the error returned says why.
	StoppedEarly string
	RunID        string
}

// orphanLog holds the orphans found by the chunk listing. Up to
// sweepOrphanMemRecords of them live on the heap; past that they are
// appended to a temp file and read back batch by batch, so memory does not
// grow with the number of orphans. Apply-only: a dry run keeps just the
// counters and the sample.
type orphanLog struct {
	dir   string
	mem   []orphanInfo
	f     *os.File
	w     *bufio.Writer
	nFile int
	err   error
}

const orphanRecLen = len(Hash{}) + 8

func (l *orphanLog) add(o orphanInfo) error {
	if l.err != nil {
		return l.err
	}
	l.mem = append(l.mem, o)
	if len(l.mem) < sweepOrphanMemRecords {
		return nil
	}
	if l.f == nil {
		f, err := os.CreateTemp(l.dir, "pghs-orphans-*")
		if err != nil {
			l.err = fmt.Errorf("repo: gc: spill orphan list: %w", err)
			return l.err
		}
		l.f, l.w = f, bufio.NewWriterSize(f, 1<<20)
	}
	var rec [orphanRecLen]byte
	for _, m := range l.mem {
		copy(rec[:], m.hash[:])
		binary.LittleEndian.PutUint64(rec[len(m.hash):], uint64(m.size))
		if _, err := l.w.Write(rec[:]); err != nil {
			l.err = fmt.Errorf("repo: gc: spill orphan list: %w", err)
			return l.err
		}
	}
	l.nFile += len(l.mem)
	l.mem = l.mem[:0]
	return nil
}

// batches calls fn with consecutive slices of at most n orphans: the spilled
// records first (in the order found), then the heap tail. fn must not keep
// the slice. A list that never spilled is sorted by hash first, so small
// sweeps delete in a deterministic order.
func (l *orphanLog) batches(n int, fn func(batch []orphanInfo) error) error {
	if l.err != nil {
		return l.err
	}
	if l.f == nil {
		sort.Slice(l.mem, func(i, j int) bool { return l.mem[i].hash.Compare(l.mem[j].hash) < 0 })
	} else {
		if err := l.w.Flush(); err != nil {
			return fmt.Errorf("repo: gc: spill orphan list: %w", err)
		}
		r := bufio.NewReaderSize(io.NewSectionReader(l.f, 0, int64(l.nFile)*int64(orphanRecLen)), 1<<20)
		buf := make([]orphanInfo, 0, n)
		var rec [orphanRecLen]byte
		for i := 0; i < l.nFile; i++ {
			if _, err := io.ReadFull(r, rec[:]); err != nil {
				return fmt.Errorf("repo: gc: read orphan list: %w", err)
			}
			var o orphanInfo
			copy(o.hash[:], rec[:len(o.hash)])
			o.size = int64(binary.LittleEndian.Uint64(rec[len(o.hash):]))
			buf = append(buf, o)
			if len(buf) == n {
				if err := fn(buf); err != nil {
					return err
				}
				buf = buf[:0]
			}
		}
		if len(buf) > 0 {
			if err := fn(buf); err != nil {
				return err
			}
		}
	}
	for lo := 0; lo < len(l.mem); lo += n {
		if err := fn(l.mem[lo:min(lo+n, len(l.mem))]); err != nil {
			return err
		}
	}
	return nil
}

func (l *orphanLog) close() {
	if l.f != nil {
		name := l.f.Name()
		_ = l.f.Close()
		_ = os.Remove(name)
		l.f = nil
	}
}

// hashSample keeps the smallest `limit` hashes it is offered.
type hashSample struct {
	limit int
	buf   []Hash
}

func (s *hashSample) offer(h Hash) {
	s.buf = append(s.buf, h)
	if len(s.buf) >= 2*s.limit {
		s.trim()
	}
}

func (s *hashSample) trim() {
	sortHashes(s.buf)
	if len(s.buf) > s.limit {
		s.buf = s.buf[:s.limit]
	}
}

func (s *hashSample) sorted() []Hash {
	s.trim()
	return s.buf
}

// noteFailure counts a failed delete and keeps the first maxSweepFailures.
func (r *SweepResult) noteFailure(msg string) {
	r.FailureCount++
	if len(r.Failures) < maxSweepFailures {
		r.Failures = append(r.Failures, msg)
	}
}

// manifestSnapshot is the reference set plus what was harvested, so a
// later rescan only has to read manifests that are new or changed.
//
// harvested is a bounded-memory set of digests of (key, size, mtime), not a
// map keyed by manifest key: a deployment archiving WAL for years holds tens
// of millions of segment manifests, and every one of them is remembered.
type manifestSnapshot struct {
	refs      *RefSet
	harvested *spill.HashSet
	opts      CollectReferencesOptions
	stale     []string
	minAge    time.Duration
	ageCutoff time.Time
}

// newManifestSnapshot builds a snapshot whose reference set and harvest
// memo both spill to refSpill.Dir. Close it when done.
func newManifestSnapshot(refSpill spill.Options, opts CollectReferencesOptions) *manifestSnapshot {
	memo := spill.Options{Dir: refSpill.Dir, MemEntries: 1 << 18}
	if refSpill.MemEntries > 0 && refSpill.MemEntries < memo.MemEntries {
		memo.MemEntries = refSpill.MemEntries
	}
	return &manifestSnapshot{
		refs:      NewRefSetWithOptions(refSpill),
		harvested: spill.NewHashSet(memo),
		opts:      opts,
	}
}

// close releases both sets' spill files.
func (s *manifestSnapshot) close() {
	_ = s.refs.Close()
	_ = s.harvested.Close()
}

// harvestDigest identifies one observed version of a manifest object.
func harvestDigest(info storage.ObjectInfo) spill.Key {
	h := sha256.New()
	var n [12]byte
	_, _ = h.Write([]byte(info.Key))
	binary.LittleEndian.PutUint64(n[:8], uint64(info.Size))
	binary.LittleEndian.PutUint32(n[8:], uint32(info.ModTime.Nanosecond()))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(n[:])
	var sec [8]byte
	binary.LittleEndian.PutUint64(sec[:], uint64(info.ModTime.Unix()))
	_, _ = h.Write(sec[:])
	var out spill.Key
	h.Sum(out[:0])
	return out
}

// harvestConcurrency bounds the parallel manifest reads of one scan.
const harvestConcurrency = 16

type harvestJob struct {
	key  string
	kind harvestKind
	info storage.ObjectInfo
}

// scan walks manifests/, wal/ and logical/ ONCE each and harvests every
// live manifest not already harvested with the same size and mtime.
// Stale staging temps (manifests/ and wal/) are detected in the same
// walk when collectStale is set. Manifests are read concurrently.
//
// Memory is independent of the number of manifests: backup manifests
// (hundreds) are held as keys, but WAL and logical-stream segment
// manifests (potentially tens of millions) are handed to the workers as
// the listing produces them, through a bounded channel.
func (s *manifestSnapshot) scan(ctx context.Context, sp storage.StoragePlugin, collectStale bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		jobs     = make(chan harvestJob, harvestConcurrency)
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}
	for i := 0; i < harvestConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					continue // drain
				}
				if err := harvestManifest(ctx, sp, j.key, s.refs, j.kind); err != nil {
					fail(err)
					continue
				}
				s.harvested.Add(harvestDigest(j.info))
			}
		}()
	}
	send := func(j harvestJob) bool {
		select {
		case jobs <- j:
			return true
		case <-ctx.Done():
			return false
		}
	}

	lerr := s.listAndDispatch(ctx, sp, collectStale, send)
	close(jobs)
	wg.Wait()

	mu.Lock()
	err := firstErr
	mu.Unlock()
	if err != nil {
		return err
	}
	if lerr != nil {
		return lerr
	}
	// A harvest memo that lost a spill write or read can answer "already
	// harvested" for a manifest that never was: refs would be short.
	if err := s.harvested.Err(); err != nil {
		return fmt.Errorf("harvest memo unreliable: %w", err)
	}
	return ctx.Err()
}

// listAndDispatch produces the harvest jobs. It returns when the listings
// are exhausted or send reports cancellation.
func (s *manifestSnapshot) listAndDispatch(ctx context.Context, sp storage.StoragePlugin, collectStale bool, send func(harvestJob) bool) error {
	grace := s.opts.effectiveGrace()
	graceCutoff := s.opts.effectiveNow().Add(-grace)

	var stale []string
	tombstoned := map[string]struct{}{}
	var backupKeys []storage.ObjectInfo

	for info, err := range sp.List(ctx, "manifests/") {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		switch {
		case strings.HasSuffix(info.Key, "/manifest.json.tombstone"):
			if grace > 0 && (info.ModTime.IsZero() || info.ModTime.After(graceCutoff)) {
				continue // young tombstone: the manifest stays live
			}
			// Deployment + id, never the id alone (see CollectReferences).
			parts := strings.Split(info.Key, "/")
			if len(parts) >= 4 {
				tombstoned[parts[1]+"/"+parts[3]] = struct{}{}
			}
		case strings.HasSuffix(info.Key, "/manifest.json"):
			backupKeys = append(backupKeys, info)
		case collectStale && isStaleTempKey(info.Key):
			if s.isStaleAged(info) {
				stale = append(stale, info.Key)
			}
		}
	}
	for _, info := range backupKeys {
		parts := strings.Split(info.Key, "/")
		if len(parts) >= 5 {
			if _, dead := tombstoned[parts[1]+"/"+parts[3]]; dead {
				continue
			}
		}
		if s.fresh(info) {
			continue
		}
		if !send(harvestJob{info.Key, harvestBackup, info}) {
			return ctx.Err()
		}
	}
	for _, prefix := range []string{"wal/", "logical/"} {
		for info, err := range sp.List(ctx, prefix) {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if isStaleTempKey(info.Key) {
				if collectStale && prefix == "wal/" && s.isStaleAged(info) {
					stale = append(stale, info.Key)
				}
				continue
			}
			if !strings.HasSuffix(info.Key, ".json") || s.fresh(info) {
				continue
			}
			if !send(harvestJob{info.Key, harvestWAL, info}) {
				return ctx.Err()
			}
		}
	}
	if collectStale {
		sort.Strings(stale)
		s.stale = stale
	}
	return nil
}

// fresh reports whether key was harvested before with identical
// size and mtime — a manifest rewritten in place (kms rotate) is
// harvested again; refs only ever grow, which is the safe direction.
func (s *manifestSnapshot) fresh(info storage.ObjectInfo) bool {
	return s.harvested.Has(harvestDigest(info))
}

func (s *manifestSnapshot) isStaleAged(info storage.ObjectInfo) bool {
	if s.minAge <= 0 {
		return true
	}
	return !info.ModTime.IsZero() && !info.ModTime.After(s.ageCutoff)
}

type orphanInfo struct {
	hash Hash
	size int64
}

// Sweep computes the orphan set and, with Apply, deletes it under the gc
// exclusion protocol (gcfence.go). Performance shape (a shared
// repository of several heavy deployments took 17+ minutes before):
//
//   - manifests/, wal/ and logical/ are each listed ONCE per scan, and
//     stale staging temps are detected in that same walk;
//   - chunk sizes and mtimes come from the single chunks/ listing, so
//     no orphan is Stat'd (neither for the reclaim estimate nor before
//     its delete);
//   - the pre-delete rescans are incremental: only manifests that are
//     new or changed since the previous scan are read, and one fence
//     checkpoint (one rescan) covers up to BatchDuration of deletes, so
//     the listing cost is not paid once per 4096 chunks;
//   - manifests are read concurrently, hashes sort by bytes.
func Sweep(ctx context.Context, sp storage.StoragePlugin, opts SweepOptions) (*SweepResult, error) {
	clock := opts.Now
	if clock == nil {
		clock = time.Now
	}
	start := clock().UTC()
	res := &SweepResult{}

	var fence *gcRun
	if opts.Apply {
		// Publish the run record FIRST, so the settle window overlaps
		// the initial walk instead of adding to it.
		var err error
		fence, err = startGCRun(ctx, sp, start)
		if err != nil {
			return nil, fmt.Errorf("repo: gc: publish run record: %w", err)
		}
		res.RunID = fence.rec.RunID
		defer fence.finish(context.WithoutCancel(ctx), opts.OnWarning)
		if opts.LiveLeases != nil {
			live, err := opts.LiveLeases(ctx)
			if err != nil {
				return nil, fmt.Errorf("repo: gc: scan backup leases: %w", err)
			}
			if len(live) > 0 {
				return nil, fmt.Errorf("%w for: %s", ErrSweepBackupInFlight, strings.Join(live, ", "))
			}
		}
	}

	if opts.Apply {
		reapStaleSpill(opts.RefSpill.Dir, start, opts.OnWarning)
	}

	fo := FindOrphansOptions{MinAge: opts.MinChunkAge, Now: start}
	minAge := fo.effectiveMinAge()
	snap := newManifestSnapshot(opts.RefSpill, CollectReferencesOptions{TombstoneGrace: opts.TombstoneGrace, Now: start})
	snap.minAge = minAge
	snap.ageCutoff = start.Add(-minAge)
	defer snap.close()
	if err := snap.scan(ctx, sp, true); err != nil {
		return nil, fmt.Errorf("repo: gc: collect references: %w", err)
	}
	// A spill I/O failure means a negative Has may be wrong; deleting on
	// that basis would destroy live chunks, so refuse to continue.
	if err := snap.refs.Err(); err != nil {
		return nil, fmt.Errorf("repo: gc: reference set unreliable: %w", err)
	}

	// One chunks/ listing: orphan counters, a bounded sample for the
	// report and, on Apply, the orphan log (spilled past a heap budget) the
	// delete loop reads back in batches. Nothing here grows with the number
	// of orphans.
	olog := &orphanLog{dir: opts.RefSpill.Dir}
	defer olog.close()
	sample := hashSample{limit: SweepOrphanSampleLimit}
	for info, err := range sp.List(ctx, "chunks/sha256/") {
		if err != nil {
			return nil, fmt.Errorf("repo: gc: list chunks: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h, perr := ParseChunkKey(info.Key)
		if perr != nil || snap.refs.Has(h) {
			continue
		}
		if minAge > 0 && (info.ModTime.IsZero() || info.ModTime.After(snap.ageCutoff)) {
			continue
		}
		// Never a chunk younger than this run, floor or no floor: it
		// was written after gc started, by a writer whose manifest the
		// snapshot cannot have seen.
		if !info.ModTime.IsZero() && info.ModTime.After(start) {
			continue
		}
		res.OrphanCount++
		res.OrphanBytes += info.Size
		sample.offer(h)
		if opts.Apply {
			if err := olog.add(orphanInfo{hash: h, size: info.Size}); err != nil {
				return nil, err
			}
		}
	}
	res.RefCount = snap.refs.Len()
	res.StaleTemps = snap.stale
	res.Orphans = sample.sorted()
	if !opts.Apply {
		return res, nil
	}

	// Settle: a writer that read "no gc running" just before our record
	// appeared has GCFenceWriterBudget to commit; the deciding snapshot
	// must come after that.
	if wait := GCFenceSettle - time.Since(fence.publishedAt); wait > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}

	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = 4096
	}
	conc := opts.DeleteConcurrency
	if conc <= 0 {
		conc = 16
	}
	// How long one checkpoint may keep deleting. A writer that pinned after
	// the checkpoint's pin read waits for the NEXT checkpoint (seq+1), i.e.
	// for the rescan and every delete of this one, and gives up after
	// GCFenceMaxWait; the whole checkpoint is therefore held to 3/5 of it.
	slice := opts.BatchDuration
	if slice == 0 && opts.BatchSize <= 0 {
		slice = GCFenceMaxWait / 5
	}
	maxCheckpoint := GCFenceMaxWait * 3 / 5
	pins := newGCPinSet(opts.RefSpill)
	defer pins.close()
	var mu sync.Mutex
	remaining := res.OrphanCount
	b := 0
	var deadline time.Time // end of the open checkpoint's delete window
	warnedSlow := false
	checkpoint := func() error {
		began := time.Now()
		// Checkpoint: publish seq, THEN read pins and rescan.
		if err := fence.advance(ctx); err != nil {
			return fmt.Errorf("repo: gc: publish batch checkpoint (refusing to delete without the fence): %w", err)
		}
		if opts.beforeBatch != nil {
			opts.beforeBatch(b)
		}
		if err := pins.refresh(ctx, sp, clock()); err != nil {
			return fmt.Errorf("repo: gc: read writer pins: %w", err)
		}
		if err := pins.err(); err != nil {
			return fmt.Errorf("repo: gc: writer pin set unreliable: %w", err)
		}
		if opts.LiveLeases != nil {
			live, err := opts.LiveLeases(ctx)
			if err != nil {
				return fmt.Errorf("repo: gc: re-scan backup leases: %w", err)
			}
			if len(live) > 0 {
				res.StoppedEarly = "backup started: " + strings.Join(live, ", ")
				return fmt.Errorf("%w (started during the sweep) for: %s; %d orphan(s) left for the next run",
					ErrSweepBackupInFlight, strings.Join(live, ", "), remaining)
			}
		}
		if err := snap.scan(ctx, sp, false); err != nil {
			return fmt.Errorf("repo: gc: rescan references before delete: %w", err)
		}
		if err := snap.refs.Err(); err != nil {
			return fmt.Errorf("repo: gc: reference set unreliable: %w", err)
		}
		if opts.beforeDelete != nil {
			opts.beforeDelete(b)
		}
		spent := time.Since(began)
		window := max(slice, 0)
		if left := maxCheckpoint - spent; left < window {
			window = max(left, 0)
		}
		if spent > maxCheckpoint {
			if !warnedSlow && opts.OnWarning != nil {
				warnedSlow = true
				opts.OnWarning(fmt.Sprintf("gc: the manifest rescan took %s, more than 3/5 of the writer fence wait (%s); a committing backup may be told to retry", spent.Round(time.Second), GCFenceMaxWait))
			}
		}
		deadline = time.Now().Add(window)
		b++
		return nil
	}
	berr := olog.batches(batchSize, func(batch []orphanInfo) error {
		// Open a new checkpoint unless the current one still has delete
		// time left. Pins read at its start stay valid for every slice
		// under it: a writer pinning later waits for the NEXT checkpoint,
		// which is published only after these deletes finished.
		if !time.Now().Before(deadline) {
			if err := checkpoint(); err != nil {
				return err
			}
		}
		if err := fence.checkFresh(); err != nil {
			return err
		}
		sem := make(chan struct{}, conc)
		var wg sync.WaitGroup
		for _, o := range batch {
			if snap.refs.Has(o.hash) || pins.has(o.hash) {
				mu.Lock()
				res.SkippedReferenced++
				mu.Unlock()
				continue
			}
			sem <- struct{}{}
			wg.Add(1)
			go func(o orphanInfo) {
				defer func() { <-sem; wg.Done() }()
				// Lazy: an orphan's deletion need not survive a crash
				// (a resurrected orphan is unreferenced and the next run
				// reaps it), and on fs the durable form costs a directory
				// fsync per chunk — the bulk of a large sweep.
				err := storage.DeleteLazy(ctx, sp, ChunkKey(o.hash))
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					res.noteFailure(fmt.Sprintf("%s: %v", o.hash, err))
					return
				}
				res.Deleted++
				res.DeletedBytes += o.size
			}(o)
		}
		wg.Wait()
		remaining -= len(batch)
		return ctx.Err()
	})
	if berr != nil {
		return res, berr
	}

	// Stale staging files. Best-effort: a tmp under an active object
	// lock cannot be deleted until it expires; a later run reaps it.
	for _, key := range snap.stale {
		if err := sp.Delete(ctx, key); err != nil {
			res.noteFailure(fmt.Sprintf("%s: %v", key, err))
			continue
		}
		res.StaleTempDeleted++
	}

	// Empty directories the deletes left behind (fs: chunks/sha256/xx/yy
	// shells, the id directory of a reaped manifest temp). Best-effort.
	// Always, not only after deleting: a run that crashed or hit faults after
	// its deletes must still be cleaned up by the next one.
	for _, prefix := range []string{"chunks", "manifests", "wal", "logical"} {
		if _, err := storage.PruneEmptyDirsOf(ctx, sp, prefix, false); err != nil && opts.OnWarning != nil {
			opts.OnWarning(fmt.Sprintf("prune empty %s directories: %v", prefix, err))
		}
	}

	// Backend staging files (.deferred-/.excl-/.hstmp-) a crashed writer
	// left behind. The backends hide them from List, so neither the
	// orphan sweep nor the stale-manifest pass can see them and they
	// leaked forever. Only temps older than DefaultStagingReapAge are
	// touched — an in-flight write's temp is minutes old. Best-effort: a
	// failure here is reported, never allowed to fail gc.
	if st, err := storage.ReapStagingOf(ctx, sp, storage.DefaultStagingReapAge); err != nil {
		if opts.OnWarning != nil {
			opts.OnWarning(fmt.Sprintf("reap backend staging files: %v", err))
		}
	} else {
		res.StagingReaped = st.Removed
		res.StagingReapedBytes = st.Bytes
	}
	sort.Strings(res.Failures)
	return res, nil
}

// gcRun is gc's live run record.
type gcRun struct {
	sp          storage.StoragePlugin
	key         string
	mu          sync.Mutex
	rec         gcRunRecord
	publishedAt time.Time // monotonic
	lastBeat    time.Time // monotonic, last successful record write
	stopBeat    chan struct{}
	beatDone    chan struct{}
}

func startGCRun(ctx context.Context, sp storage.StoragePlugin, start time.Time) (*gcRun, error) {
	host, _ := os.Hostname()
	id := newFenceID()
	if host != "" {
		id += "-" + sanitizeRunHost(host)
	}
	r := &gcRun{
		sp:  sp,
		key: gcRunsPrefix + id + ".json",
		rec: gcRunRecord{Schema: gcRunSchema, RunID: id, State: "running", StartedAt: start},
	}
	if err := r.write(ctx); err != nil {
		return nil, err
	}
	r.publishedAt = time.Now()
	r.stopBeat = make(chan struct{})
	r.beatDone = make(chan struct{})
	go r.heartbeat(context.WithoutCancel(ctx))
	return r, nil
}

func sanitizeRunHost(h string) string {
	var b strings.Builder
	for _, c := range h {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// write publishes the record (caller may hold mu).
func (r *gcRun) write(ctx context.Context) error {
	now := time.Now().UTC()
	r.rec.HeartbeatAt = now
	r.rec.ExpiresAt = now.Add(GCRunTTL)
	if err := putJSON(ctx, r.sp, r.key, r.rec); err != nil {
		return err
	}
	r.lastBeat = time.Now()
	return nil
}

func (r *gcRun) heartbeat(ctx context.Context) {
	defer close(r.beatDone)
	t := time.NewTicker(GCRunTTL / 4)
	defer t.Stop()
	for {
		select {
		case <-r.stopBeat:
			return
		case <-t.C:
			r.mu.Lock()
			_ = r.write(ctx) // a missed beat is caught by checkFresh
			r.mu.Unlock()
		}
	}
}

// advance publishes the next batch checkpoint (seq+1).
func (r *gcRun) advance(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rec.Seq++
	return r.write(ctx)
}

// checkFresh refuses to delete when our own record may have expired in
// writers' eyes (a stalled process, a backend that rejected heartbeats):
// a writer that saw it expired did not pin, so deleting now is unsafe.
func (r *gcRun) checkFresh() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.lastBeat) > GCRunTTL/2 {
		return fmt.Errorf("repo: gc: run record not refreshed for %s (heartbeats failing?); stopping before a delete writers may not be fenced against", time.Since(r.lastBeat).Round(time.Second))
	}
	return nil
}

// finish marks the run finished and reaps expired records and pins.
func (r *gcRun) finish(ctx context.Context, warn func(string)) {
	close(r.stopBeat)
	<-r.beatDone
	r.mu.Lock()
	r.rec.State = "finished"
	err := r.write(ctx)
	r.mu.Unlock()
	if err != nil && warn != nil {
		warn(fmt.Sprintf("could not mark gc run %s finished (%v); writers treat it as live until it expires in %s", r.rec.RunID, err, GCRunTTL))
	}
	now := time.Now()
	for info, lerr := range r.sp.List(ctx, gcPinsPrefix) {
		if lerr != nil {
			break
		}
		var pin gcPin
		if gerr := getJSON(ctx, r.sp, info.Key, &pin); gerr == nil && now.Sub(pin.CreatedAt) > GCPinTTL {
			_ = r.sp.Delete(ctx, info.Key)
		}
	}
	for info, lerr := range r.sp.List(ctx, gcRunsPrefix) {
		if lerr != nil {
			break
		}
		if info.Key == r.key {
			continue
		}
		var rec gcRunRecord
		if gerr := getJSON(ctx, r.sp, info.Key, &rec); gerr == nil && !rec.liveAt(now) &&
			now.Sub(rec.HeartbeatAt) > gcRunRecordRetention {
			_ = r.sp.Delete(ctx, info.Key)
		}
	}
}

// staleSpillAge is how old spill leftovers must be to be reaped. A gc killed
// with SIGKILL/OOM never runs its deferred Close, and its spill files then sat
// in the temp dir forever. Spill run files are written once and not touched
// again, so this is far beyond the lifetime of any live sweep; a live set that
// lost its files fails closed (Err), it never under-counts.
const staleSpillAge = 7 * 24 * time.Hour

// reapStaleSpill removes pghs-spill-* directories and pghs-orphans-* files
// older than staleSpillAge from dir (default: the system temp dir).
func reapStaleSpill(dir string, now time.Time, warn func(string)) {
	if dir == "" {
		dir = os.TempDir()
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-staleSpillAge)
	for _, e := range ents {
		n := e.Name()
		isDir := strings.HasPrefix(n, "pghs-spill-") && e.IsDir()
		if !isDir && !(strings.HasPrefix(n, "pghs-orphans-") && !e.IsDir()) {
			continue
		}
		fi, err := e.Info()
		if err != nil || fi.ModTime().After(cutoff) {
			continue
		}
		if isDir {
			// newest entry decides: a dir's own mtime moves only on create/unlink
			newest := fi.ModTime()
			sub, _ := os.ReadDir(filepath.Join(dir, n))
			for _, se := range sub {
				if si, err := se.Info(); err == nil && si.ModTime().After(newest) {
					newest = si.ModTime()
				}
			}
			if newest.After(cutoff) {
				continue
			}
		}
		if err := os.RemoveAll(filepath.Join(dir, n)); err != nil && warn != nil {
			warn(fmt.Sprintf("reap stale spill leftover %s: %v", n, err))
		}
	}
}
