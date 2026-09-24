// gcfence.go — the gc exclusion protocol: run records, pins, and the
// writer-side commit fence that makes a manifest commit over ADOPTED
// chunks safe against a concurrent `repo gc --apply`.
package repo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// THE PROBLEM
//
// gc decides "chunk X is an orphan" from a reference snapshot, then
// deletes X — possibly many minutes later. A writer (backup, WAL
// segment, replicate, bundle import) that DEDUPLICATES against X
// (adopts it without writing it: a dedup hit touches no object and
// refreshes no mtime) and then commits a manifest referencing X can
// interleave with that as:
//
//	gc:     snapshot (X orphan) ............................ delete X
//	writer:            adopt X ... re-Stat X (present) ... commit
//
// The writer's commit-time re-Stat passes, the manifest commits, and gc
// deletes a now-referenced chunk: a backup that reports success and is
// unrestorable. Re-collecting references once and scanning backup leases
// once (what gc did) only shrinks the window; WAL and logical writers
// hold no lease at all.
//
// THE PROTOCOL
//
// gc side (Sweep):
//
//  1. Before anything else, write a RUN RECORD gc/runs/<id>.json
//     {state: running, seq: 0, expires_at}. It is heartbeated while the
//     run lives and set to state=finished at the end.
//  2. Wait until GCFenceSettle has elapsed since the record was
//     written, then take the reference snapshot that decides deletions
//     (incrementally on top of the initial walk).
//  3. Before EVERY delete batch: bump seq in the run record, read every
//     PIN (gc/pins/*.json), re-check backup leases, rescan for manifests
//     committed since the snapshot — and exclude every pinned or newly
//     referenced chunk from the batch.
//  4. Never delete a chunk younger than the run's start, even with the
//     --min-chunk-age floor disabled.
//
// Writer side (BeginCommitFence → re-Stat adopted chunks → commit →
// Confirm), only when the manifest references adopted chunks:
//
//	A. Read the run records (R1).
//	B. If a gc run is live: write a PIN naming the adopted chunks, re-read
//	   the run records, and wait until every live run's seq advances (its
//	   in-flight batch has finished) or it ends.
//	C. Re-Stat the adopted chunks; commit.
//	D. Confirm: if no pin was written and more than GCFenceWriterBudget
//	   passed between R1 and the commit returning, re-read the run
//	   records; if any gc run started since R1 (or is live), pin, wait
//	   as in B, and re-Stat — a chunk missing now means the committed
//	   manifest is broken, which is reported, never hidden.
//
// WHY IT IS SOUND. Let gc run G delete X in batch j, whose pin read
// happened at Bj (after G bumped seq to j, and before the delete).
//
//   - Writer pinned (B) at Pw. If Bj > Pw, G read the pin and excluded
//     X. If Bj < Pw, then G's seq was already j when the writer re-read
//     the records after Pw, so the writer waited for seq > j — i.e.
//     until after batch j's deletes — and its re-Stat (C) sees X gone
//     and refuses to commit.
//   - Writer did not pin (no live run at R1). A run G that was live
//     before R1 and finished before R1 deleted X before R1, so the
//     re-Stat in C (after R1) sees it. A run G whose record appeared
//     after R1 takes its deciding snapshot at least GCFenceSettle after
//     that; if the writer's commit returned within GCFenceWriterBudget
//     (< GCFenceSettle) of R1, the snapshot sees the committed manifest
//     and X is referenced. Otherwise Confirm (D) sees G's record, and
//     the same argument as the pinned case applies to the post-commit
//     re-Stat.
//
// Timing enters only as DURATIONS measured on one host each (the
// writer's budget, gc's settle), never as a comparison between two
// clocks — except for run-record expiry, which, exactly like backup
// leases, assumes hosts agree on the time to well within GCRunTTL.
//
// Pins are never deleted by writers: a pin's only job is to be seen by
// any batch that starts after it, and gc reaps pins older than
// GCPinTTL. They are written only while a gc run is live, so there are
// few of them.

const (
	gcRunsPrefix = "gc/runs/"
	gcPinsPrefix = "gc/pins/"
)

// Tunables. Variables rather than constants so tests can compress the
// timing; production code never changes them. GCFenceSettle MUST exceed
// GCFenceWriterBudget by a healthy margin — the settle is what makes a
// writer that read "no gc running" safe.
var (
	// GCFenceSettle is how long gc waits after publishing its run record
	// before taking the reference snapshot that decides deletions.
	GCFenceSettle = 60 * time.Second
	// GCFenceWriterBudget is how long a writer may take from reading the
	// run records to its commit returning and still rely on the settle.
	GCFenceWriterBudget = 30 * time.Second
	// GCRunTTL is how long a run record stays live without a heartbeat.
	GCRunTTL = 2 * time.Minute
	// GCPinTTL is how long gc honours a pin.
	GCPinTTL = 6 * time.Hour
	// GCFenceMaxWait bounds how long a writer waits for a live gc run's
	// batch to advance before giving up (retryable error).
	GCFenceMaxWait = 5 * time.Minute
	// gcFencePoll is the writer's poll interval while waiting.
	gcFencePoll = 250 * time.Millisecond
	// gcRunRecordRetention is how long finished run records are kept.
	gcRunRecordRetention = 24 * time.Hour
)

// gcRunRecord is the on-storage run record.
type gcRunRecord struct {
	Schema      string    `json:"schema"`
	RunID       string    `json:"run_id"`
	State       string    `json:"state"` // "running" | "finished"
	Seq         int64     `json:"seq"`
	StartedAt   time.Time `json:"started_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

const gcRunSchema = "pg_hardstorage.gc_run.v1"

func (r gcRunRecord) liveAt(now time.Time) bool {
	return r.State == "running" && now.Before(r.ExpiresAt)
}

// gcPin is the on-storage pin: chunks a committing writer adopted.
type gcPin struct {
	Schema    string    `json:"schema"`
	Owner     string    `json:"owner,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Hashes    []Hash    `json:"hashes"`
}

const gcPinSchema = "pg_hardstorage.gc_pin.v1"

func newFenceID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

func putJSON(ctx context.Context, sp storage.StoragePlugin, key string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = sp.Put(ctx, key, bytes.NewReader(body), storage.PutOptions{ContentLength: int64(len(body))})
	return err
}

func getJSON(ctx context.Context, sp storage.StoragePlugin, key string, v any) error {
	rc, err := sp.Get(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	body, err := storage.ReadAllLimited(rc, storage.MaxMetadataBytes)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// listGCRuns reads every run record. An unreadable record is treated as
// a LIVE run that never advances until it expires by its object age —
// failing to read the thing that says "gc is deleting" is no reason to
// assume it is not.
func listGCRuns(ctx context.Context, sp storage.StoragePlugin) (map[string]gcRunRecord, error) {
	out := map[string]gcRunRecord{}
	for info, err := range sp.List(ctx, gcRunsPrefix) {
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(info.Key, ".json") {
			continue
		}
		var r gcRunRecord
		if gerr := getJSON(ctx, sp, info.Key, &r); gerr != nil {
			if errors.Is(gerr, storage.ErrNotFound) {
				continue
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			r = gcRunRecord{RunID: info.Key, State: "running", ExpiresAt: info.ModTime.Add(GCRunTTL)}
			if info.ModTime.IsZero() {
				r.ExpiresAt = time.Now().Add(GCRunTTL)
			}
		}
		if r.RunID == "" {
			r.RunID = info.Key
		}
		out[info.Key] = r
	}
	return out, nil
}

// ErrAdoptedChunkSwept reports that a chunk a writer deduplicated against
// was deleted by gc before (or, in the post-commit check, after) the
// commit — see the protocol above.
var ErrAdoptedChunkSwept = errors.New("repo: adopted chunk deleted by a concurrent gc")

// FenceOptions tunes a commit fence.
type FenceOptions struct {
	// Owner names the committing writer in pins (diagnostics only).
	Owner string
}

// CommitFence is the writer half of the gc exclusion protocol for ONE
// manifest commit. Obtain it with BeginCommitFence BEFORE re-Statting
// the adopted chunks, commit, then call Confirm.
type CommitFence struct {
	sp       storage.StoragePlugin
	adopted  []Hash
	owner    string
	start    time.Time // monotonic, taken before the R1 read
	seenRuns map[string]struct{}
	pinned   bool
	pinnedAt time.Time
}

// BeginCommitFence starts the fence for a commit that references the
// given adopted chunks. With no adopted chunks it is a no-op (a chunk
// the writer wrote itself is protected by gc's age floor and by never
// deleting chunks younger than the gc run). When a gc run is live it
// pins the chunks and waits for the run's in-flight delete batch to
// finish — bounded by GCFenceMaxWait. The caller MUST re-Stat the
// adopted chunks after this returns and before committing.
func BeginCommitFence(ctx context.Context, sp storage.StoragePlugin, adopted []Hash, opts FenceOptions) (*CommitFence, error) {
	f := &CommitFence{sp: sp, adopted: append([]Hash(nil), adopted...), owner: opts.Owner, start: time.Now()}
	if len(f.adopted) == 0 {
		return f, nil
	}
	runs, err := listGCRuns(ctx, sp)
	if err != nil {
		return nil, fmt.Errorf("repo: gc fence: read gc run records: %w", err)
	}
	f.seenRuns = map[string]struct{}{}
	live := false
	now := time.Now()
	for k, r := range runs {
		f.seenRuns[k] = struct{}{}
		if r.liveAt(now) {
			live = true
		}
	}
	if live {
		if err := f.pinAndWait(ctx); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// pinAndWait writes the pin, then waits for every run live AFTER the
// pin write to advance past its current batch (or end).
func (f *CommitFence) pinAndWait(ctx context.Context) error {
	if !f.pinned {
		pin := gcPin{Schema: gcPinSchema, Owner: f.owner, CreatedAt: time.Now().UTC(), Hashes: f.adopted}
		if err := putJSON(ctx, f.sp, gcPinsPrefix+newFenceID()+".json", pin); err != nil {
			return fmt.Errorf("repo: gc fence: write pin: %w", err)
		}
		f.pinned = true
		f.pinnedAt = time.Now()
	}
	runs, err := listGCRuns(ctx, f.sp)
	if err != nil {
		return fmt.Errorf("repo: gc fence: read gc run records: %w", err)
	}
	waitFor := map[string]int64{}
	now := time.Now()
	for k, r := range runs {
		f.seenRuns[k] = struct{}{}
		if r.liveAt(now) {
			waitFor[k] = r.Seq
		}
	}
	deadline := time.Now().Add(GCFenceMaxWait)
	for len(waitFor) > 0 {
		if time.Now().After(deadline) {
			return fmt.Errorf("repo: gc fence: a running `repo gc --apply` did not finish its delete batch within %s; refusing to commit over chunks it may be deleting (retry)", GCFenceMaxWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(gcFencePoll):
		}
		now = time.Now()
		for k, seq := range waitFor {
			var r gcRunRecord
			if gerr := getJSON(ctx, f.sp, k, &r); gerr != nil {
				if errors.Is(gerr, storage.ErrNotFound) {
					delete(waitFor, k)
				}
				continue
			}
			if !r.liveAt(now) || r.Seq > seq {
				delete(waitFor, k)
			}
		}
	}
	return nil
}

// Adopted returns the chunks this fence protects.
func (f *CommitFence) Adopted() []Hash { return f.adopted }

// Confirm completes the fence after the commit returned. It returns an
// error wrapping ErrAdoptedChunkSwept when the committed manifest
// references an adopted chunk that a gc run deleted — only possible when
// the commit overran GCFenceWriterBudget while a gc run started — so the
// caller can fail loudly rather than report a broken backup as good.
func (f *CommitFence) Confirm(ctx context.Context) error {
	if f == nil || len(f.adopted) == 0 {
		return nil
	}
	elapsed := time.Since(f.start)
	if f.pinned && time.Since(f.pinnedAt) < GCPinTTL/2 {
		return nil // the pin covered every batch that could matter
	}
	if !f.pinned && elapsed <= GCFenceWriterBudget {
		return nil // inside the settle window of any run that started after R1
	}
	runs, err := listGCRuns(ctx, f.sp)
	if err != nil {
		return fmt.Errorf("repo: gc fence: confirm: read gc run records: %w", err)
	}
	now := time.Now()
	started := false
	for k, r := range runs {
		if _, seen := f.seenRuns[k]; !seen || r.liveAt(now) {
			started = true
		}
	}
	if !started && elapsed < gcRunRecordRetention/2 {
		return nil // no gc run began since R1: nothing could have swept
	}
	if err := f.pinAndWait(ctx); err != nil {
		return err
	}
	var missing []string
	for _, h := range f.adopted {
		if _, serr := f.sp.Stat(ctx, ChunkKey(h)); errors.Is(serr, storage.ErrNotFound) {
			missing = append(missing, h.String())
		}
	}
	if len(missing) > 0 {
		if len(missing) > 8 {
			missing = append(missing[:8], fmt.Sprintf("… +%d more", len(missing)-8))
		}
		return fmt.Errorf("%w: the manifest was committed, but %d chunk(s) it deduplicated against were deleted by a `repo gc --apply` that started while the commit was in progress: %s — the committed manifest is NOT restorable; re-run the operation (it rewrites the chunks)",
			ErrAdoptedChunkSwept, len(missing), strings.Join(missing, ", "))
	}
	return nil
}

// FencedCommit runs the whole writer protocol around commit for callers
// that hold a CAS: fence, re-Stat the adopted chunks (dropping missing
// ones from the CAS so a retry rewrites them), commit, confirm.
func FencedCommit(ctx context.Context, sp storage.StoragePlugin, cas *CAS, adopted []Hash, opts FenceOptions, commit func(context.Context) error) (unchecked int, err error) {
	fence, err := BeginCommitFence(ctx, sp, adopted, opts)
	if err != nil {
		return 0, err
	}
	var missing []Hash
	if cas != nil {
		missing, unchecked, err = cas.VerifyAdopted(ctx, adopted)
		if err != nil {
			return unchecked, err
		}
	}
	if len(missing) > 0 {
		return unchecked, fmt.Errorf("%w before the manifest could commit: %d chunk(s); retry (the retry rewrites them)", ErrAdoptedChunkSwept, len(missing))
	}
	if err := commit(ctx); err != nil {
		return unchecked, err
	}
	return unchecked, fence.Confirm(ctx)
}

// gcPinSet is gc's cached view of the pins: pins are immutable once
// written, so each is read once per run.
type gcPinSet struct {
	mu     sync.Mutex
	read   map[string]struct{}
	hashes map[Hash]struct{}
}

func newGCPinSet() *gcPinSet {
	return &gcPinSet{read: map[string]struct{}{}, hashes: map[Hash]struct{}{}}
}

// refresh reads every pin not seen before. A pin older than GCPinTTL is
// ignored (and reaped at the end of the run). A pin that cannot be read
// fails the refresh: gc must not delete a batch while a pin it could
// not read may name those chunks.
func (p *gcPinSet) refresh(ctx context.Context, sp storage.StoragePlugin, now time.Time) error {
	for info, err := range sp.List(ctx, gcPinsPrefix) {
		if err != nil {
			return err
		}
		p.mu.Lock()
		_, done := p.read[info.Key]
		p.mu.Unlock()
		if done || !strings.HasSuffix(info.Key, ".json") {
			continue
		}
		var pin gcPin
		if gerr := getJSON(ctx, sp, info.Key, &pin); gerr != nil {
			if errors.Is(gerr, storage.ErrNotFound) {
				continue
			}
			return fmt.Errorf("read pin %s: %w", info.Key, gerr)
		}
		p.mu.Lock()
		p.read[info.Key] = struct{}{}
		if now.Sub(pin.CreatedAt) < GCPinTTL {
			for _, h := range pin.Hashes {
				p.hashes[h] = struct{}{}
			}
		}
		p.mu.Unlock()
	}
	return nil
}

func (p *gcPinSet) has(h Hash) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.hashes[h]
	return ok
}
