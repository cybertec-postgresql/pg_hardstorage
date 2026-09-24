// gcsweep.go — Sweep: the one engine behind `repo gc` and
// `repair chunks --orphans`: a single-walk reference snapshot, the
// orphan scan, and (with Apply) the fenced, batched delete.
package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
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

	// Now is the clock (default time.Now).
	Now func() time.Time

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

// SweepResult is the outcome of Sweep.
type SweepResult struct {
	RefCount         int
	Orphans          []Hash // sorted
	OrphanBytes      int64  // from the chunk listing; no per-orphan Stat
	StaleTemps       []string
	Deleted          int
	DeletedBytes     int64
	StaleTempDeleted int
	Failures         []string // per-key delete failures (uncapped)
	// SkippedReferenced counts orphans spared because a manifest
	// committed after the snapshot, or a writer's pin, claimed them.
	SkippedReferenced int
	// StoppedEarly is non-empty when Apply stopped before the last batch
	// (live backup lease); the error returned says why.
	StoppedEarly string
	RunID        string
}

// manifestSnapshot is the reference set plus what was harvested, so a
// later rescan only has to read manifests that are new or changed.
type manifestSnapshot struct {
	refs      *RefSet
	harvested map[string]storage.ObjectInfo
	opts      CollectReferencesOptions
	stale     []string
	minAge    time.Duration
	ageCutoff time.Time
}

// scan walks manifests/, wal/ and logical/ ONCE each and harvests every
// live manifest not already harvested with the same size and mtime.
// Stale staging temps (manifests/ and wal/) are detected in the same
// walk when collectStale is set. Manifests are read concurrently.
func (s *manifestSnapshot) scan(ctx context.Context, sp storage.StoragePlugin, collectStale bool) error {
	grace := s.opts.effectiveGrace()
	graceCutoff := s.opts.effectiveNow().Add(-grace)

	type job struct {
		key  string
		kind harvestKind
		info storage.ObjectInfo
	}
	var jobs []job
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
		jobs = append(jobs, job{info.Key, harvestBackup, info})
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
			jobs = append(jobs, job{info.Key, harvestWAL, info})
		}
	}

	// Concurrent harvest: on an object store each manifest is a round
	// trip, and a shared repository holds tens of thousands of them.
	const harvestConcurrency = 16
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sem      = make(chan struct{}, harvestConcurrency)
	)
	for _, j := range jobs {
		mu.Lock()
		failed := firstErr != nil
		mu.Unlock()
		if failed {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(j job) {
			defer func() { <-sem; wg.Done() }()
			if err := harvestManifest(ctx, sp, j.key, s.refs, j.kind); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			s.harvested[j.key] = j.info
			mu.Unlock()
		}(j)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if collectStale {
		sort.Strings(stale)
		s.stale = stale
	}
	return ctx.Err()
}

// fresh reports whether key was harvested before with identical
// size and mtime — a manifest rewritten in place (kms rotate) is
// harvested again; refs only ever grow, which is the safe direction.
func (s *manifestSnapshot) fresh(info storage.ObjectInfo) bool {
	prev, ok := s.harvested[info.Key]
	return ok && prev.Size == info.Size && prev.ModTime.Equal(info.ModTime)
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
//     new or changed since the previous scan are read;
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

	fo := FindOrphansOptions{MinAge: opts.MinChunkAge, Now: start}
	minAge := fo.effectiveMinAge()
	snap := &manifestSnapshot{
		refs:      NewRefSet(),
		harvested: map[string]storage.ObjectInfo{},
		opts:      CollectReferencesOptions{TombstoneGrace: opts.TombstoneGrace, Now: start},
		minAge:    minAge,
		ageCutoff: start.Add(-minAge),
	}
	if err := snap.scan(ctx, sp, true); err != nil {
		return nil, fmt.Errorf("repo: gc: collect references: %w", err)
	}

	// One chunks/ listing: orphan set with sizes.
	var orphans []orphanInfo
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
		orphans = append(orphans, orphanInfo{hash: h, size: info.Size})
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].hash.Compare(orphans[j].hash) < 0 })
	res.RefCount = snap.refs.Len()
	res.StaleTemps = snap.stale
	res.Orphans = make([]Hash, len(orphans))
	for i, o := range orphans {
		res.Orphans[i] = o.hash
		res.OrphanBytes += o.size
	}
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
	pins := newGCPinSet()
	var mu sync.Mutex
	for b, lo := 0, 0; lo < len(orphans); b, lo = b+1, lo+batchSize {
		hi := min(lo+batchSize, len(orphans))
		// Checkpoint: publish seq, THEN read pins and rescan.
		if err := fence.advance(ctx); err != nil {
			return res, fmt.Errorf("repo: gc: publish batch checkpoint (refusing to delete without the fence): %w", err)
		}
		if opts.beforeBatch != nil {
			opts.beforeBatch(b)
		}
		if err := pins.refresh(ctx, sp, clock()); err != nil {
			return res, fmt.Errorf("repo: gc: read writer pins: %w", err)
		}
		if opts.LiveLeases != nil {
			live, err := opts.LiveLeases(ctx)
			if err != nil {
				return res, fmt.Errorf("repo: gc: re-scan backup leases: %w", err)
			}
			if len(live) > 0 {
				res.StoppedEarly = "backup started: " + strings.Join(live, ", ")
				return res, fmt.Errorf("%w (started during the sweep) for: %s; %d orphan(s) left for the next run",
					ErrSweepBackupInFlight, strings.Join(live, ", "), len(orphans)-lo)
			}
		}
		if err := snap.scan(ctx, sp, false); err != nil {
			return res, fmt.Errorf("repo: gc: rescan references before delete: %w", err)
		}
		if opts.beforeDelete != nil {
			opts.beforeDelete(b)
		}
		if err := fence.checkFresh(); err != nil {
			return res, err
		}
		sem := make(chan struct{}, conc)
		var wg sync.WaitGroup
		for _, o := range orphans[lo:hi] {
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
					res.Failures = append(res.Failures, fmt.Sprintf("%s: %v", o.hash, err))
					return
				}
				res.Deleted++
				res.DeletedBytes += o.size
			}(o)
		}
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return res, err
		}
	}

	// Stale staging files. Best-effort: a tmp under an active object
	// lock cannot be deleted until it expires; a later run reaps it.
	for _, key := range snap.stale {
		if err := sp.Delete(ctx, key); err != nil {
			res.Failures = append(res.Failures, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		res.StaleTempDeleted++
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
