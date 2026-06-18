// Package audit implements pg_hardstorage's hash-chained audit log.
//
// Each audit Event records a single operator-visible action (a backup
// committed, a hold placed, a KMS rotation initiated, ...). Events
// are written into the repository alongside backup manifests at
//
//	audit/<yyyy>/<mm>/<dd>/<seq>-<id>.json
//
// The seq prefix makes lex-sorted listing return events in commit
// order, and the per-day directory keeps any single prefix bounded
// for object-store LIST efficiency.
//
// Each event carries a SHA-256 hash of its canonical JSON form AND
// the hash of the immediately-prior event. Walking the chain verifies
// that no event has been tampered with after the fact: changing any
// historical field changes that event's hash, which invalidates the
// next event's PrevHash, which cascades through to the head. This is
// the same trick git uses for commits.
//
// What's deliberately NOT here for the pull-forward:
//
//   - Rekor / transparency-log anchoring (— periodic publishing
//     of the chain head to a public log so even a fully-compromised
//     repo operator can't silently rewrite history).
//   - Durable monotonic sequence counter across restarts (today's
//     sequence is derived from the prior event's sequence + 1; on a
//     fresh repo the counter starts at 0; the per-day path layout
//     plus seq-prefix-in-filename makes "find the latest" cheap).
//   - In-database SQL views (`pg_hardstorage.audit_events`);.
package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdio "io"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// defaultAuditLoggerMu guards the package-level fallback loggers
// so SetDefault* callers (tests, agent startup) don't race with the
// goroutines emitting audit events.
var defaultAuditLoggerMu sync.RWMutex

// defaultHeadPointerErrorLogger is invoked when the audit head-
// pointer write fails inside Append.  Default writes to
// log.Default(); the failure is non-fatal (chain integrity is
// preserved by the listing-walk fallback) but it indicates either
// a transient backend issue or a permission problem the operator
// should know about. .
var defaultHeadPointerErrorLogger = func(err error) {
	log.Printf("audit: %v", err)
}

// defaultAppendErrorLogger is invoked by AppendOrLog when the
// underlying Append fails.  Default writes to log.Default().
var defaultAppendErrorLogger = func(err error) {
	log.Printf("audit: %v", err)
}

// SetDefaultHeadPointerErrorLogger replaces the package-level
// fallback.  Pass nil to disable.  Returns the prior logger.
func SetDefaultHeadPointerErrorLogger(fn func(error)) func(error) {
	defaultAuditLoggerMu.Lock()
	defer defaultAuditLoggerMu.Unlock()
	prior := defaultHeadPointerErrorLogger
	defaultHeadPointerErrorLogger = fn
	return prior
}

// SetDefaultAppendErrorLogger replaces the package-level fallback
// for AppendOrLog.  Pass nil to disable.  Returns the prior logger.
func SetDefaultAppendErrorLogger(fn func(error)) func(error) {
	defaultAuditLoggerMu.Lock()
	defer defaultAuditLoggerMu.Unlock()
	prior := defaultAppendErrorLogger
	defaultAppendErrorLogger = fn
	return prior
}

func currentHeadPointerErrorLogger() func(error) {
	defaultAuditLoggerMu.RLock()
	defer defaultAuditLoggerMu.RUnlock()
	return defaultHeadPointerErrorLogger
}

func currentAppendErrorLogger() func(error) {
	defaultAuditLoggerMu.RLock()
	defer defaultAuditLoggerMu.RUnlock()
	return defaultAppendErrorLogger
}

// Schema is the on-disk version tag for Event bodies.
const Schema = "pg_hardstorage.audit.v1"

// GenesisHash is the value PrevHash takes for the very first event
// in a new chain (i.e. an empty repo's first audit append). 64 zeros
// in hex; chosen because it's both visually distinguishable and
// trivially refusable as a real SHA-256 output.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// HeadKey is the path to the head-pointer file. Stored alongside the
// chain so a single GET turns the O(N) "walk-and-find-the-tail" into
// an O(1) lookup. The file holds a HeadPointer JSON document; a
// missing file (404) means the chain is empty OR the cache hasn't
// been populated yet, and Append falls back to the listing walk to
// rebuild it. The pointer is observability + perf, not correctness:
// VerifyChain re-derives the chain from scratch and would catch any
// pointer/chain divergence.
const HeadKey = "audit/_head.json"

// HeadPointer is the persisted head-cache. JSON-encoded at HeadKey.
// Schema-versioned independently from the audit Event so a future
// pointer schema change doesn't require migrating every event.
type HeadPointer struct {
	Schema    string    `json:"schema"`
	Sequence  int64     `json:"sequence"` // matches the head event's Sequence
	Hash      string    `json:"hash"`     // matches the head event's Hash
	EventID   string    `json:"event_id"` // for triage; cross-references the event file
	Key       string    `json:"key"`      // canonical event-file path; lets us read the head with one GET
	UpdatedAt time.Time `json:"updated_at"`
}

// HeadPointerSchema is the on-disk version tag for HeadPointer.
const HeadPointerSchema = "pg_hardstorage.audit.head.v1"

// Event is one row in the audit log.
type Event struct {
	Schema    string         `json:"schema"`
	ID        string         `json:"id"`        // ULID-shaped or UUID; opaque
	Sequence  int64          `json:"sequence"`  // monotonic; 0 for the first event
	Timestamp time.Time      `json:"timestamp"` // RFC3339 UTC
	Actor     string         `json:"actor,omitempty"`
	Tenant    string         `json:"tenant,omitempty"`
	Action    string         `json:"action"` // dotted: backup.create, kms.rotate, ...
	Subject   Subject        `json:"subject,omitempty"`
	Body      map[string]any `json:"body,omitempty"`
	PrevHash  string         `json:"prev_hash"`
	Hash      string         `json:"hash"`
}

// Subject is the per-event identity tuple (parallels output.Subject;
// duplicated here so audit doesn't depend on the output package).
type Subject struct {
	Deployment string `json:"deployment,omitempty"`
	BackupID   string `json:"backup_id,omitempty"`
	Tenant     string `json:"tenant,omitempty"`
	Repo       string `json:"repo,omitempty"`
}

// canonicalForHash returns the canonical JSON for hashing — same
// shape as the event but with Hash zeroed (so the field's own value
// doesn't influence its hash) and PrevHash preserved (so tampering
// with PrevHash invalidates the chain).
//
// We use json.MarshalIndent="" to keep the bytes deterministic across
// Go versions; the encoder's key order is alphabetical for marshalled
// structs, which gives us stable output without writing a custom
// canonicalizer.
func canonicalForHash(ev *Event) ([]byte, error) {
	clone := *ev
	clone.Hash = ""
	return json.Marshal(&clone)
}

// ComputeHash is defined in computehash.go (production) and
// computehash_mutation_*.go (testkit-mutation variants under
// specific build tags).  Extracting it lets the testkit swap in
// deliberately-broken variants without disturbing this file.

// keyFor returns the on-disk key for an event. The seq prefix in the
// filename is zero-padded so lex-sorted lists are commit-ordered;
// 16 hex digits = 64-bit sequence, plenty.
//
// Global chain:  audit/2026/04/29/0000000000000001-01H7K8...json
// Sharded chain: audit/shards/d.db1/2026/04/29/0000...-01H7K8...json
// keyFor is the storage key of an event. It is a function of (shard,
// Sequence) ONLY — the ID and timestamp live in the event body, not the
// key. That is what lets concurrent appends serialize via the
// IfNotExists Put in Append (a fixed sequence ⇒ a fixed key ⇒ a real
// collision the loser can detect and retry past). hexSeq is fixed-width
// zero-padded, so the keys also sort lexicographically by Sequence.
//
// Earlier revisions bucketed events under a YYYY/MM/DD path and suffixed
// the unique event ID; that made every concurrent append land on its own
// key and silently fork the chain (see Append). Legacy chains may still
// hold "<date>/<hexSeq>-<id>.json" keys; the reader paths (listShard...,
// head's fallback, verifyShard) key off the body's Sequence and parse
// the leading hexSeq from the filename, so both layouts interoperate.
func keyFor(ev *Event) string {
	return shardEventDir(shardKeyFor(ev)) + hexSeq(ev.Sequence) + ".json"
}

// seqFromEventKey extracts an event's Sequence from its storage key
// filename, accepting both the current "<hexSeq>.json" layout and the
// legacy "<date>/<hexSeq>-<id>.json" layout (the hexSeq is always the
// leading 16 hex chars of the basename). Used by head()'s fallback to
// find the chain tip by Sequence rather than trusting lexical key order,
// which a legacy chain's date-first path or a mixed-layout chain can
// violate.
func seqFromEventKey(key string) (int64, bool) {
	base := key
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".json")
	if i := strings.IndexByte(base, '-'); i >= 0 {
		base = base[:i]
	}
	if len(base) != 16 {
		return 0, false
	}
	n, err := strconv.ParseInt(base, 16, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func hexSeq(n int64) string {
	const width = 16
	out := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		out[i] = "0123456789abcdef"[n&0xF]
		n >>= 4
	}
	return string(out)
}

// --- Audit chain sharding ------------------------------------------
//
// The audit log is partitioned into independent hash chains — "shards"
// — so appends to different scopes never contend on one shared head
// pointer and one global sequence (the serialization point that made a
// single per-repo chain a bottleneck for large fleets).
//
// Each shard is its OWN tamper-evident chain: its own head pointer, its
// own monotonic sequence, its own PrevHash linkage. An event's scope
// (Subject.Deployment / Tenant) is part of its canonical hash, so it
// cannot be relocated into another shard without breaking its own hash;
// VerifyChain additionally asserts every event sits in the shard its
// scope implies (catching wholesale-relocation of an internally
// consistent sub-chain).
//
// The empty shard "" is the GLOBAL chain and keeps the exact legacy
// on-disk layout (audit/<yyyy>/... + audit/_head.json), so a repo
// written before sharding stays valid with no migration — its events
// simply remain the global chain, and newly-scoped events start landing
// in their own shards.
const shardsRoot = "audit/shards/"

// shardKeyFor derives an event's shard from the most specific scope it
// carries: deployment, then tenant, then the global chain (""). This is
// the ONLY dimension-dependent piece of the sharding machinery.
func shardKeyFor(ev *Event) string {
	if d := ev.Subject.Deployment; d != "" {
		return "d." + sanitizeShardSegment(d)
	}
	if t := ev.Subject.Tenant; t != "" {
		return "t." + sanitizeShardSegment(t)
	}
	if t := ev.Tenant; t != "" {
		return "t." + sanitizeShardSegment(t)
	}
	return ""
}

// sanitizeShardSegment keeps a shard a single, well-formed path
// segment. Deployment names are already validated path-safe upstream;
// this is defence in depth for tenant strings and programmatic callers.
// Path separators and control characters collapse to '_' — two scopes
// differing only in such a character would merge into one chain (still
// tamper-evident, just combined), which is acceptable for names that
// should never contain them.
func sanitizeShardSegment(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, s)
}

// shardEventDir is the directory prefix under which a shard's event
// files live. Global ("") → the legacy "audit/" root; a named shard →
// "audit/shards/<shard>/".
func shardEventDir(shard string) string {
	if shard == "" {
		return "audit/"
	}
	return shardsRoot + shard + "/"
}

// headKeyForShard is the head-pointer key for a shard. Global → the
// legacy HeadKey; a named shard → its own "_head.json".
func headKeyForShard(shard string) string {
	if shard == "" {
		return HeadKey
	}
	return shardsRoot + shard + "/_head.json"
}

// HeadKeyForShard is the exported storage key of a shard's head pointer.
// Callers outside the package (e.g. doctor's anchor-freshness probe) use
// it to read a shard's authoritative current head sequence directly,
// rather than re-deriving it by counting event files — a count that is
// wrong under WORM retention pruning (oldest events are reaped while
// sequence numbers climb) and across shards.
func HeadKeyForShard(shard string) string { return headKeyForShard(shard) }

// Store reads + writes the audit log against any StoragePlugin.
// When `worm` is set, every committed Event is locked under the
// repo's retention policy. The head-pointer cache (HeadKey) is
// deliberately NOT subject to retention — it's a perf cache,
// rewritten on every Append, and regenerable from the event
// listing; locking it would block every Append.
type Store struct {
	sp   storage.StoragePlugin
	worm *repo.WORMPolicy
}

// NewStore wraps sp without WORM threading. Audit events go in
// without retention; suitable for non-regulated repos.
func NewStore(sp storage.StoragePlugin) *Store {
	if sp == nil {
		panic("audit: NewStore requires a non-nil StoragePlugin")
	}
	return &Store{sp: sp}
}

// NewStoreWithRetention wraps sp + applies the WORM retention
// policy to every committed event. When policy is nil/zero this
// is identical to NewStore(sp).
//
// Per-event retention semantics: each Append captures `now` at
// commit time, so a long-running agent's audit log keeps rolling
// retention rather than locking everything to the agent's start
// timestamp. Backends without Object-Lock (fs) ignore the field.
//
// The head-pointer Put deliberately does NOT carry retention.
// HeadKey is rewritten on every Append (last-writer-wins), and
// WORM would refuse the rewrite. The head pointer is regenerable
// from the event listing — losing it doesn't compromise audit
// integrity. The events themselves are what carry the chain trust.
func NewStoreWithRetention(sp storage.StoragePlugin, policy *repo.WORMPolicy) *Store {
	if sp == nil {
		panic("audit: NewStoreWithRetention requires a non-nil StoragePlugin")
	}
	return &Store{sp: sp, worm: policy}
}

// Append finalizes ev (sets PrevHash, Sequence, Hash) and writes it
// to the repo. ev is mutated in place.
//
// Append is O(1) on the steady-state path: it reads
// `audit/_head.json` to find the prior event's sequence + hash,
// computes the new event, writes it, then updates `_head.json` in
// place. Cache miss (no pointer yet, or it's stale/corrupt) falls
// back to listing the audit/ prefix to find the true tail; the
// pointer rebuild is the next action's amortised cost.
//
// Why the pointer is OK to be eventually-consistent: VerifyChain
// re-derives the chain from raw events and reports any pointer
// divergence as part of the result. The pointer is a perf
// optimisation, not a trust input — the chain hashes are the
// trust input, and they live in the events themselves.
func (s *Store) Append(ctx context.Context, ev *Event) error {
	if ev.Schema == "" {
		ev.Schema = Schema
	}
	if ev.Action == "" {
		return errAuditField("audit: Action is required")
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	if ev.ID == "" {
		ev.ID = newEventID(ev.Timestamp)
	}

	shard := shardKeyFor(ev)
	prev, err := s.head(ctx, shard)
	if err != nil {
		return err
	}
	var prevHash string
	var seq int64
	if prev == nil {
		prevHash = GenesisHash
		seq = 0
	} else {
		prevHash = prev.Hash
		seq = prev.Sequence + 1
	}

	// Optimistic, lock-free append. The event's storage key is a
	// function of (shard, sequence) ONLY — the ID and timestamp live in
	// the body, not the key — so two racing appends that pick the same
	// sequence collide on the IfNotExists Put: exactly one wins the
	// slot. The loser reads the *winning* event, re-links onto it, and
	// retries the next slot.
	//
	// This serializes the chain across goroutines AND processes without
	// any lock, and crucially WITHOUT forking it. The previous key
	// embedded the unique event ID, so concurrent appends produced two
	// distinct keys at the same Sequence/PrevHash — both Puts succeeded
	// and the chain forked. VerifyChain re-derives in Sequence order and
	// a fork makes link N+1's PrevHash mismatch its sorted predecessor,
	// so honest concurrency was reported as a tamper break.
	//
	// Crash-safety: the durable event IS the slot claim (a single
	// conditional write), so there are no orphaned reservations — a
	// crash between the Put and the head-pointer write just leaves a
	// committed event the next Append rediscovers via head().
	//
	// Re-reading the winner (not the head pointer, which may lag a
	// concurrent appender's not-yet-written update) guarantees both a
	// correct PrevHash and strict forward progress: each iteration
	// advances seq past a slot we observed taken, so the loop is bounded
	// by the number of concurrent appenders.
	for {
		ev.PrevHash = prevHash
		ev.Sequence = seq

		h, err := ComputeHash(ev)
		if err != nil {
			return err
		}
		ev.Hash = h

		body, err := json.MarshalIndent(ev, "", "  ")
		if err != nil {
			return err
		}
		key := keyFor(ev)
		putOpts := storage.PutOptions{
			ContentLength: int64(len(body)),
			IfNotExists:   true,
		}
		if !s.worm.IsZero() {
			now := time.Now().UTC()
			putOpts.RetainUntil = s.worm.RetainUntil(now)
			putOpts.RetentionMode = storage.WORMMode(s.worm.Mode)
		}
		_, err = s.sp.Put(ctx, key, bytes.NewReader(body), putOpts)
		if err == nil {
			// Won the slot. Update the head pointer. A failure here is
			// non-fatal — the event itself is committed; the next Append
			// will rebuild the pointer from the listing walk.
			//
			// previously the failure was silently
			// swallowed. We now route it through the package-level
			// defaultHeadPointerErrorLogger (writes to log.Default by
			// default) so an operator running with the default config
			// sees the warning rather than discovering it later via
			// degraded Append performance.
			if err := s.writeHeadPointer(ctx, shard, ev, key); err != nil {
				if logger := currentHeadPointerErrorLogger(); logger != nil {
					logger(fmt.Errorf("audit: head pointer write failed (chain integrity intact, next Append will rebuild): %w", err))
				}
			}
			return nil
		}
		if !errors.Is(err, storage.ErrAlreadyExists) {
			return err
		}
		// Lost the race for this sequence. Read the event that won the
		// slot, link onto it, and try the next one.
		winner, gerr := s.getByKey(ctx, key)
		if gerr != nil {
			return gerr
		}
		prevHash = winner.Hash
		seq = winner.Sequence + 1
	}
}

// AppendOrLog wraps Append + routes any error through the
// package-level defaultAppendErrorLogger.  An audit noted that
// the codebase routinely uses `_ = store.Append(ctx, ev)` for
// "best-effort" audit emission — losing the error means audit-
// chain gaps go unnoticed until `audit verify-chain` catches them.
//
// AppendOrLog preserves the best-effort posture (callers don't
// have to error-handle) while ensuring failures are visible.
// Default logger writes to log.Default(); tests / agents override
// via SetDefaultAppendErrorLogger.
func (s *Store) AppendOrLog(ctx context.Context, ev *Event) {
	if err := s.Append(ctx, ev); err != nil {
		if logger := currentAppendErrorLogger(); logger != nil {
			action := ""
			if ev != nil {
				action = ev.Action
			}
			logger(fmt.Errorf("audit: append %q failed (chain may have a gap): %w", action, err))
		}
	}
}

// writeHeadPointer persists `audit/_head.json` so the next Append's
// head() lookup is O(1). Best-effort — the pointer is observability +
// perf; the chain remains trustworthy without it.
func (s *Store) writeHeadPointer(ctx context.Context, shard string, ev *Event, key string) error {
	hp := HeadPointer{
		Schema:    HeadPointerSchema,
		Sequence:  ev.Sequence,
		Hash:      ev.Hash,
		EventID:   ev.ID,
		Key:       key,
		UpdatedAt: time.Now().UTC(),
	}
	body, err := json.MarshalIndent(&hp, "", "  ")
	if err != nil {
		return err
	}
	// Pointer is rewritten on every Append; IfNotExists would defeat
	// the purpose. Plain Put is fine — last-writer-wins is the
	// intended semantics, and a stale pointer survives because the
	// fallback walk catches it. Each shard has its own pointer, so
	// appends to different shards never contend on this write.
	_, err = s.sp.Put(ctx, headKeyForShard(shard), bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	})
	return err
}

// readHeadPointer fetches the cached head, returning nil when absent
// (chain empty or cache not yet populated). Errors that aren't
// "not found" surface — a corrupt pointer is worth investigating.
func (s *Store) readHeadPointer(ctx context.Context, shard string) (*HeadPointer, error) {
	rc, err := s.sp.Get(ctx, headKeyForShard(shard))
	if err != nil {
		// Not-found is the cache-miss path; treat as nil so head()
		// falls back to the listing walk.
		if errors.Is(err, storage.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	defer rc.Close()
	body, err := stdio.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	var hp HeadPointer
	if err := json.Unmarshal(body, &hp); err != nil {
		// A corrupt pointer is treated as a cache miss; fallback
		// rebuilds it on the next Append.
		return nil, nil
	}
	if hp.Schema != HeadPointerSchema {
		// Future-pointer schema we don't understand — fall back to
		// the listing walk rather than guessing.
		return nil, nil
	}
	return &hp, nil
}

// head returns the latest event (highest sequence) or nil if the
// chain is empty.
//
// O(1) on the steady-state path via the head pointer at HeadKey;
// O(N) on the cache-miss path (no pointer, stale pointer, or its
// referenced event vanished). The cache-miss path rebuilds the
// pointer as a side effect of the next Append.
func (s *Store) head(ctx context.Context, shard string) (*Event, error) {
	if hp, err := s.readHeadPointer(ctx, shard); err == nil && hp != nil && hp.Key != "" {
		ev, err := s.getByKey(ctx, hp.Key)
		if err == nil {
			// Sanity guard: if the pointer's hash doesn't match the
			// event's actual hash, the pointer is stale or
			// corrupt — fall back to the walk.
			if ev != nil && ev.Hash == hp.Hash && ev.Sequence == hp.Sequence {
				return ev, nil
			}
		}
		// Pointer stale or unreadable; fall through to the walk.
	}
	// Fallback: list this shard's events and pick the chain tip by
	// Sequence. We parse the Sequence from each key's filename rather
	// than trusting lexical key order — a legacy date-bucketed chain
	// sorts by date, not Sequence (a backward wall-clock step across a
	// day boundary would mis-order it), and a chain that spans the old
	// and new key layouts does not sort by Sequence at all. The current
	// "<hexSeq>.json" layout does sort correctly, but keying off the
	// parsed Sequence is correct for every layout.
	all, err := s.listShardEventKeys(ctx, shard)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	tail := ""
	var tailSeq int64 = -1
	for _, k := range all {
		seq, ok := seqFromEventKey(k)
		if !ok {
			continue
		}
		if tail == "" || seq > tailSeq {
			tail, tailSeq = k, seq
		}
	}
	if tail == "" {
		// No parseable key (unexpected); fall back to the lex-tail so we
		// still return a head rather than nil.
		tail = all[len(all)-1]
	}
	return s.getByKey(ctx, tail)
}

// allKeysSorted is `head()`'s slow-path lister; it must skip the
// head pointer file (which lives at audit/_head.json) so the lex tail
// is a real event, not the pointer's own key. The pointer's "audit/"
// prefix puts it in the listing, but the suffix-trim filter at the
// caller already drops non-.json files; _head.json IS a .json file,
// so we filter explicitly here.

// listAllKeysSorted lists every audit/*.json key in lex order, EXCEPT
// non-event sidecar files that live alongside the chain in the same
// prefix:
//
//   - audit/_head.json: the chain-tail pointer.
//   - audit/anchors/<log-id>.json: transparency-log anchor records
//     (own schema `pg_hardstorage.audit.anchor.v1`).  Without this
//     filter, anchors get picked up by Search and VerifyChain, then
//     unmarshal into zero-valued Event structs — which inflates the
//     Search count by 1 per anchor and breaks VerifyChain's hash
//     integrity (the zero event's empty prev_hash won't match the
//     prior event's hash).  Surfaced by L8_audit_anchor_round_trip
//     after the first `audit anchor` write made `audit verify-chain`
//     report `verify.audit_chain_broken: 1 hash mismatch(es), 1
//     chain break(s)` on a chain that was green moments earlier.
//
// Per-day directory layout means the natural prefix walk is
// sequence-sorted.
//
// listShardEventKeys lists one shard's event keys in lex (= commit)
// order. The global chain ("") shares the audit/ root with the sharded
// sub-tree, the head pointer, and the anchor sidecars, so it excludes
// all of them; a named shard's directory contains only its own events
// plus its _head.json.
func (s *Store) listShardEventKeys(ctx context.Context, shard string) ([]string, error) {
	dir := shardEventDir(shard)
	headKey := headKeyForShard(shard)
	var out []string
	for info, err := range s.sp.List(ctx, dir) {
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(info.Key, ".json") {
			continue
		}
		if info.Key == headKey {
			continue
		}
		if shard == "" {
			// The global chain must not pick up sharded events or
			// anchor sidecars that live under the same audit/ root.
			if strings.HasPrefix(info.Key, shardsRoot) {
				continue
			}
			if strings.HasPrefix(info.Key, AnchorPrefix) {
				continue
			}
		}
		out = append(out, info.Key)
	}
	sort.Strings(out)
	return out, nil
}

// listShards enumerates every chain in the repo: the global chain ("")
// plus each named shard discovered under audit/shards/. Sorted, with
// the global chain first.
func (s *Store) listShards(ctx context.Context) ([]string, error) {
	shards := []string{""}
	seen := map[string]struct{}{"": {}}
	for info, err := range s.sp.List(ctx, shardsRoot) {
		if err != nil {
			return nil, err
		}
		rel := strings.TrimPrefix(info.Key, shardsRoot)
		i := strings.IndexByte(rel, '/')
		if i <= 0 {
			continue
		}
		shard := rel[:i]
		if _, ok := seen[shard]; ok {
			continue
		}
		seen[shard] = struct{}{}
		shards = append(shards, shard)
	}
	sort.Strings(shards)
	return shards, nil
}

// getByKey reads + parses one event by full key.
func (s *Store) getByKey(ctx context.Context, key string) (*Event, error) {
	rc, err := s.sp.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := stdio.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// ListFilters is the Search query. Filters AND-combine: an event
// must match every non-zero criterion to be included.
//
// Action vs ActionPrefix is the common forensic split: "show me
// every backup.delete" (Action) vs "show me everything that
// happened in the backup namespace" (ActionPrefix="backup.").
// Setting both is allowed but redundant — Action is the strict
// match, ActionPrefix is the loose match; an Action that doesn't
// share the ActionPrefix matches nothing.
type ListFilters struct {
	Action        string    // exact match (e.g. "backup.create")
	ActionPrefix  string    // dot-namespaced prefix (e.g. "backup.")
	Actor         string    // exact match
	ActorContains string    // substring match — partial principal lookups
	Tenant        string    // exact match
	Deployment    string    // exact match against ev.Subject.Deployment
	BackupID      string    // exact match against ev.Subject.BackupID
	Since         time.Time // events at or after this timestamp
	Until         time.Time // events strictly before this timestamp
	Limit         int       // 0 = all
	Reverse       bool      // newest-first ordering when true (commit order otherwise)
}

// Search walks the chain and returns events matching filters in
// commit order (or reverse-commit order when Reverse is set).
// For huge audit logs this is O(N);+ adds an index file
// alongside the chain.
//
// Limit interaction with Reverse: Limit caps AFTER ordering, so
// Reverse=true + Limit=10 returns the 10 most-recent events
// matching filters — the natural "what happened recently?" query.
func (s *Store) Search(ctx context.Context, f ListFilters) ([]*Event, error) {
	shards, err := s.listShards(ctx)
	if err != nil {
		return nil, err
	}
	// Collect matches across every shard, then order globally. Per-shard
	// Sequence isn't comparable across shards, so the global order is by
	// Timestamp (ID breaks ties deterministically) — the multi-shard
	// equivalent of the single-chain commit order. Limit/Reverse apply
	// AFTER the global sort so they mean "the newest/oldest N overall".
	var out []*Event
	for _, shard := range shards {
		keys, err := s.listShardEventKeys(ctx, shard)
		if err != nil {
			return out, err
		}
		for _, k := range keys {
			ev, err := s.getByKey(ctx, k)
			if err != nil {
				return out, err
			}
			if matches(ev, f) {
				out = append(out, ev)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Timestamp.Equal(out[j].Timestamp) {
			return out[i].Timestamp.Before(out[j].Timestamp)
		}
		return out[i].ID < out[j].ID
	})
	if f.Reverse {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func matches(ev *Event, f ListFilters) bool {
	if f.Action != "" && ev.Action != f.Action {
		return false
	}
	if f.ActionPrefix != "" && !strings.HasPrefix(ev.Action, f.ActionPrefix) {
		return false
	}
	if f.Actor != "" && ev.Actor != f.Actor {
		return false
	}
	if f.ActorContains != "" && !strings.Contains(ev.Actor, f.ActorContains) {
		return false
	}
	if f.Tenant != "" && ev.Tenant != f.Tenant {
		return false
	}
	if f.Deployment != "" && ev.Subject.Deployment != f.Deployment {
		return false
	}
	if f.BackupID != "" && ev.Subject.BackupID != f.BackupID {
		return false
	}
	if !f.Since.IsZero() && ev.Timestamp.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && !ev.Timestamp.Before(f.Until) {
		return false
	}
	return true
}

// SummaryByAction groups Search results by Action and returns
// per-action counts. Used by `audit summary` for the
// compliance-style "what happened to this deployment in the last
// 30 days?" rollup. Same filter shape as Search; the only
// difference is the result aggregates rather than returns
// individual events.
//
// Returns (countByAction, totalMatched, error). The total covers
// every event matching filters, even if the per-action map has
// many or few keys.
func (s *Store) SummaryByAction(ctx context.Context, f ListFilters) (map[string]int, int, error) {
	// Force unlimited iteration — Limit semantics for a summary
	// would be confusing ("limit before grouping" vs "limit
	// groups"); the summary is meant to count EVERYTHING that
	// matches.
	loop := f
	loop.Limit = 0
	loop.Reverse = false
	events, err := s.Search(ctx, loop)
	if err != nil {
		return nil, 0, err
	}
	counts := make(map[string]int, 8)
	for _, ev := range events {
		counts[ev.Action]++
	}
	return counts, len(events), nil
}

// VerifyResult records the outcome of VerifyChain.
type VerifyResult struct {
	EventsChecked  int      `json:"events_checked"`
	HashMismatches []string `json:"hash_mismatches,omitempty"` // event IDs whose recomputed hash disagrees with stored
	ChainBreaks    []string `json:"chain_breaks,omitempty"`    // event IDs whose PrevHash != prior event's Hash (within its shard)
	Misfiled       []string `json:"misfiled,omitempty"`        // event IDs filed under a shard their scope doesn't imply (relocation signal)
	OK             bool     `json:"ok"`
}

// VerifyChain walks every event in commit order, recomputing hashes
// and asserting:
//
//   - event.Hash matches the SHA-256 of canonicalForHash(event)
//   - event.PrevHash matches the prior event's Hash
//
// The first event's PrevHash must equal GenesisHash. Both findings
// surface in the result; ChainBreaks AND HashMismatches together
// can fire (a tampered event invalidates both its own hash and the
// next event's prev_hash).
func (s *Store) VerifyChain(ctx context.Context) (VerifyResult, error) {
	res := VerifyResult{}
	shards, err := s.listShards(ctx)
	if err != nil {
		return res, err
	}
	// Each shard is an independent chain; verify them all and aggregate.
	// Findings carry the offending event IDs, which are globally unique,
	// so the operator can locate them regardless of shard.
	for _, shard := range shards {
		if err := s.verifyShard(ctx, shard, &res); err != nil {
			return res, err
		}
	}
	res.OK = len(res.HashMismatches) == 0 && len(res.ChainBreaks) == 0 && len(res.Misfiled) == 0
	return res, nil
}

// verifyShard checks one shard's chain and folds its findings into res.
func (s *Store) verifyShard(ctx context.Context, shard string, res *VerifyResult) error {
	keys, err := s.listShardEventKeys(ctx, shard)
	if err != nil {
		return err
	}
	// The storage keys sort date-first (.../YYYY/MM/DD/<seq>-<id>), so a
	// backward wall-clock step across a day boundary can place a
	// higher-Sequence event under an earlier date and reorder the walk
	// — which would flag a perfectly valid chain as broken. The
	// authoritative append order is the monotonic Sequence; do the hash
	// self-check while streaming (cheap, per-event), but collect a small
	// (seq, hash, prev_hash, id) tuple per event and run the PrevHash
	// linkage check in Sequence order.
	type link struct {
		seq            int64
		hash, prev, id string
	}
	var links []link
	for _, k := range keys {
		ev, err := s.getByKey(ctx, k)
		if err != nil {
			return err
		}
		res.EventsChecked++
		recomputed, err := ComputeHash(ev)
		if err != nil {
			return err
		}
		if recomputed != ev.Hash {
			res.HashMismatches = append(res.HashMismatches, ev.ID)
		}
		// Belongs-in-shard: an event whose scope implies a different
		// shard has been relocated into this chain — a tamper signal
		// even when its own hash is self-consistent (e.g. a whole
		// internally-valid sub-chain moved wholesale into another
		// shard's directory, which the linkage check alone would miss).
		if shardKeyFor(ev) != shard {
			res.Misfiled = append(res.Misfiled, ev.ID)
		}
		links = append(links, link{seq: ev.Sequence, hash: ev.Hash, prev: ev.PrevHash, id: ev.ID})
	}
	sort.Slice(links, func(i, j int) bool { return links[i].seq < links[j].seq })
	for i, l := range links {
		expectedPrev := GenesisHash
		if i > 0 {
			expectedPrev = links[i-1].hash
		}
		if l.prev != expectedPrev {
			res.ChainBreaks = append(res.ChainBreaks, l.id)
		}
	}
	return nil
}

// Anchor walks the chain to find the head event, computes its hash
// + sequence, and posts the resulting Anchor to the supplied
// TransparencyLog. Returns the anchor (with LogID populated) so the
// caller can render it.
//
// Idempotency: re-anchoring the same chain head is a no-op (the log
// derives a deterministic ID from the head hash). Anchoring after a
// new event was committed produces a fresh log entry — the chain
// has moved forward.
//
// Anchor is NOT a critical-path operation. It runs from a periodic
// job (e.g. once an hour from the schedule subsystem) or
// manually via `pg_hardstorage audit anchor`. A failed anchor must
// not block backups or the rest of the chain — surface the error
// for monitoring and let the next anchor cycle catch up.
func (s *Store) Anchor(ctx context.Context, log TransparencyLog, publisherID string) (*Anchor, error) {
	a, err := s.anchorShard(ctx, log, publisherID, "")
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, errAuditField("audit: global chain is empty; nothing to anchor")
	}
	return a, nil
}

// AnchorAll witnesses the head of EVERY shard (the global chain plus
// each named shard) into the transparency log, so the entire
// multi-shard audit state is externally anchored. Returns one Anchor
// per non-empty shard; an empty repo yields an empty slice (not an
// error). Idempotent per shard — re-anchoring an unchanged head returns
// that shard's existing log ID.
func (s *Store) AnchorAll(ctx context.Context, log TransparencyLog, publisherID string) ([]*Anchor, error) {
	shards, err := s.listShards(ctx)
	if err != nil {
		return nil, err
	}
	var out []*Anchor
	for _, shard := range shards {
		a, err := s.anchorShard(ctx, log, publisherID, shard)
		if err != nil {
			return out, err
		}
		if a != nil {
			out = append(out, a)
		}
	}
	return out, nil
}

// anchorShard witnesses one shard's head, or returns (nil, nil) when
// that shard has no events.
func (s *Store) anchorShard(ctx context.Context, log TransparencyLog, publisherID, shard string) (*Anchor, error) {
	head, err := s.head(ctx, shard)
	if err != nil {
		return nil, err
	}
	if head == nil {
		return nil, nil
	}
	a := Anchor{
		Schema:        AnchorSchema,
		Shard:         shard,
		ChainHeadHash: head.Hash,
		HeadSequence:  head.Sequence,
		AnchoredAt:    time.Now().UTC(),
		PublisherID:   publisherID,
	}
	logID, err := log.PutAnchor(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("audit: put anchor: %w", err)
	}
	a.LogID = logID
	return &a, nil
}

// VerifyAnchorResult records the outcome of VerifyAnchor.
type VerifyAnchorResult struct {
	LogID             string `json:"log_id"`
	ChainHeadHash     string `json:"chain_head_hash"`
	HeadSequence      int64  `json:"head_sequence"`
	LocalHeadHash     string `json:"local_head_hash"`
	LocalHeadSequence int64  `json:"local_head_sequence"`
	OK                bool   `json:"ok"`
	Mismatch          string `json:"mismatch,omitempty"`
}

// VerifyAnchor reads the named anchor from the log, walks the local
// chain to the same sequence, and asserts:
//
//   - the local event at HeadSequence has Hash == anchor.ChainHeadHash
//
// Failure modes:
//
//   - chain shorter than the anchor (the local chain has been
//     truncated below the anchored head)
//   - hash mismatch at the anchored sequence (the local chain has
//     been rewritten since the anchor)
//
// Both surface as Mismatch text + OK=false so the caller can
// distinguish "verified" from "tampered" without parsing English.
func (s *Store) VerifyAnchor(ctx context.Context, log TransparencyLog, logID string) (VerifyAnchorResult, error) {
	res := VerifyAnchorResult{LogID: logID}
	a, err := log.GetAnchor(ctx, logID)
	if err != nil {
		return res, fmt.Errorf("audit: read anchor %s: %w", logID, err)
	}
	if a == nil {
		return res, errAuditField("audit: anchor not found")
	}
	res.ChainHeadHash = a.ChainHeadHash
	res.HeadSequence = a.HeadSequence

	// Walk the anchored shard's chain to find the event at the anchored
	// sequence. a.Shard is "" for both the global chain and pre-sharding
	// anchors, so legacy anchors verify against the global chain exactly
	// as before.
	keys, err := s.listShardEventKeys(ctx, a.Shard)
	if err != nil {
		return res, err
	}
	if int64(len(keys)) <= a.HeadSequence {
		res.Mismatch = fmt.Sprintf("local chain is shorter than the anchor (have %d events, anchor points at sequence %d)",
			len(keys), a.HeadSequence)
		return res, nil
	}
	// The global chain's events are sequence-contiguous, and the keys
	// sort in sequence order, so index = sequence (events are 0-indexed).
	ev, err := s.getByKey(ctx, keys[a.HeadSequence])
	if err != nil {
		return res, err
	}
	res.LocalHeadHash = ev.Hash
	res.LocalHeadSequence = ev.Sequence
	if ev.Hash != a.ChainHeadHash {
		res.Mismatch = fmt.Sprintf("hash mismatch at sequence %d: local=%s anchor=%s",
			a.HeadSequence, ev.Hash, a.ChainHeadHash)
		return res, nil
	}
	res.OK = true
	return res, nil
}

// auditFieldErr is a tiny error type so misuse surfaces with a
// stable identifier and not just a string the tests may grep for.
type auditFieldErr struct{ msg string }

// Error returns the field-validation message.
func (e *auditFieldErr) Error() string { return e.msg }
func errAuditField(msg string) error   { return &auditFieldErr{msg} }

// IsFieldError reports whether err originated from field-level
// validation in this package (vs. an I/O or chain failure).
func IsFieldError(err error) bool { _, ok := err.(*auditFieldErr); return ok }

// newEventID builds a 26-char ID: 10 hex of UTC nanoseconds + 16
// hex of crypto-random. Sortable by time, unique enough that two
// events at the same nanosecond don't collide. We avoid the ULID
// dep so audit stays in stdlib-only territory.
func newEventID(t time.Time) string {
	const width = 26
	id := make([]byte, width)
	// 10 hex of nanoseconds-since-epoch (lower 40 bits is ~36 hours
	// of nanoseconds; upper 24 bits captures the date). Concatenate.
	nanos := uint64(t.UTC().UnixNano())
	for i := 9; i >= 0; i-- {
		id[i] = "0123456789abcdef"[nanos&0xF]
		nanos >>= 4
	}
	// 16 hex of random bytes (8 bytes of entropy).
	rb := randomBytes(8)
	for i, b := range rb {
		id[10+i*2] = "0123456789abcdef"[b>>4]
		id[10+i*2+1] = "0123456789abcdef"[b&0xF]
	}
	return string(id)
}

// randomBytes is a small wrapper so tests can substitute via the
// OverrideRand hook (in audit_test_hooks.go). Production reads
// crypto/rand; tests can pin a deterministic source.
var randomBytes = realRandomBytes

func realRandomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := randRead(b); err != nil {
		// crypto/rand failure is essentially never observed on a
		// working OS, but in an AUDIT subsystem of all places we do
		// not silently return zero bytes — that would collide with
		// every other zero-bytes call and produce duplicate event
		// IDs, breaking the hash chain's uniqueness guarantee. We
		// panic so the agent restarts (the supervisor's documented
		// recovery path) rather than continue writing un-unique IDs.
		panic("audit: crypto/rand failed: " + err.Error())
	}
	return b
}

// indirected so the rand reader can be swapped in tests without
// pulling crypto/rand into them. Variable is overridden via the
// test_hooks file.
var randRead = cryptoRandRead
