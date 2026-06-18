// Package gapstate persists WAL-gap diagnostics into the
// repository so they survive agent restarts and become visible
// to inspection commands (`doctor`, future `wal gaps`).
//
// Concept: every time the leader-follow Coordinator detects a
// non-zero WAL gap (Mechanism 2 / Mechanism 3 dual-slot), it
// writes a record at:
//
//	wal/<deployment>/gaps/<tli>-<unix-nanos>.json
//
// The records are append-only — each detection is a fresh entry
// rather than overwriting a single "current gap" file. That
// preserves the forensic trail and lets `doctor` show "the last
// 5 detected gaps in the past 24h" without us having to maintain
// a rotating window. GC's job, when retention sweeps the
// timeline, is to reap gaps for tombstoned manifests; that's a
// future commit.
//
// Reading: List walks the prefix and returns parsed Records
// newest-first. Latest returns the most recent or nil if none
// exist on the timeline.
//
// What this package does NOT do:
//   - Decide whether a gap is acceptable. The Coordinator
//     emits the structured wal_gap_detected event; this is the
//     persistent record alongside.
//   - Refuse PITR within the gap. The restore-time refusal
//     (plan §"Restore preview ... refuses if a gap covers the
//     target") is a separate piece that consults this package
//   - the manifest's WAL-required range.
package gapstate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdio "io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Schema is the wire-format identifier carried on every Record.
// 24-month back-compat commitment matches the rest of the v1
// schema set.
const Schema = "pg_hardstorage.wal.gap.v1"

// Record is one persisted WAL-gap detection.
type Record struct {
	Schema      string    `json:"schema"`
	Deployment  string    `json:"deployment"`
	SlotName    string    `json:"slot_name"`
	SlotRole    string    `json:"slot_role,omitempty"`
	Timeline    uint32    `json:"timeline"`
	GapStartLSN string    `json:"gap_start_lsn"`
	GapEndLSN   string    `json:"gap_end_lsn"`
	GapBytes    uint64    `json:"gap_bytes"`
	DetectedAt  time.Time `json:"detected_at"`
}

// Store wraps a StoragePlugin with the gap-record CRUD.
type Store struct {
	sp  storage.StoragePlugin
	now func() time.Time
}

// New wraps sp. now defaults to time.Now (tests can override
// for deterministic key generation).
func New(sp storage.StoragePlugin) *Store {
	if sp == nil {
		panic("gapstate: nil StoragePlugin")
	}
	return &Store{sp: sp, now: time.Now}
}

// NewWithClock returns a Store whose key generation uses the
// supplied clock instead of time.Now. Used in tests so the
// per-record key (which embeds nanos) is reproducible.
func NewWithClock(sp storage.StoragePlugin, now func() time.Time) *Store {
	if sp == nil {
		panic("gapstate: nil StoragePlugin")
	}
	if now == nil {
		now = time.Now
	}
	return &Store{sp: sp, now: now}
}

// Put writes a gap record at:
//
//	wal/<deployment>/gaps/<tli>-<unix-nanos>.json
//
// Schema is auto-stamped if empty. DetectedAt defaults to the
// store's clock when zero. Returns the canonical key on
// success.
func (s *Store) Put(ctx context.Context, r Record) (string, error) {
	if r.Deployment == "" {
		return "", errors.New("gapstate: empty deployment")
	}
	if r.Timeline == 0 {
		return "", errors.New("gapstate: TLI 0 is invalid")
	}
	if r.SlotName == "" {
		return "", errors.New("gapstate: empty slot name")
	}
	if r.GapBytes == 0 {
		return "", errors.New("gapstate: refusing to record a zero-byte gap (no signal)")
	}
	if r.Schema == "" {
		r.Schema = Schema
	}
	if r.DetectedAt.IsZero() {
		r.DetectedAt = s.now().UTC()
	} else {
		r.DetectedAt = r.DetectedAt.UTC()
	}

	key := keyFor(r.Deployment, r.Timeline, r.DetectedAt)
	body, err := json.MarshalIndent(&r, "", "  ")
	if err != nil {
		return "", fmt.Errorf("gapstate: encode record: %w", err)
	}
	// IfNotExists: keys embed the unix-nano of the detection,
	// so collisions effectively can't happen — but the flag is
	// safety-belt against a clock-jump-backward situation.
	if _, err := s.sp.Put(ctx, key, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
		IfNotExists:   true,
	}); err != nil {
		return "", fmt.Errorf("gapstate: put %s: %w", key, err)
	}
	return key, nil
}

// keyedRecord pairs a parsed Record with the exact storage key it
// was read from. Purge paths delete by this key rather than
// reconstructing one from the record body — a reconstructed key can
// drift from the real one (hand-edited body, schema evolution of the
// key layout) and silently miss the delete while reporting success.
type keyedRecord struct {
	key string
	rec Record
}

// listKeyed walks every parseable gap record under
// wal/<deployment>/gaps/ and returns it paired with its storage key,
// newest-first by DetectedAt. Unparseable records are skipped (see
// List); callers needing a complete key sweep regardless of
// parseability use listRawKeys.
func (s *Store) listKeyed(ctx context.Context, deployment string) ([]keyedRecord, error) {
	prefix := prefixFor(deployment)
	var out []keyedRecord
	for info, err := range s.sp.List(ctx, prefix) {
		if err != nil {
			return nil, fmt.Errorf("gapstate: list %s: %w", prefix, err)
		}
		if !strings.HasSuffix(info.Key, ".json") {
			continue
		}
		rec, err := s.read(ctx, info.Key)
		if err != nil {
			// One corrupt record shouldn't lock out the rest.
			// Skip; doctor will surface the corruption via the
			// audit-chain integrity check separately.
			continue
		}
		out = append(out, keyedRecord{key: info.Key, rec: rec})
	}
	// Newest-first.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].rec.DetectedAt.After(out[j].rec.DetectedAt)
	})
	return out, nil
}

// listRawKeys returns every .json gap key under the deployment
// prefix, regardless of whether its body parses. PurgeAll uses this
// so a corrupt or schema-drifted record is still wiped — the
// end-to-end wipe contract must not leave orphan files behind.
func (s *Store) listRawKeys(ctx context.Context, deployment string) ([]string, error) {
	prefix := prefixFor(deployment)
	var keys []string
	for info, err := range s.sp.List(ctx, prefix) {
		if err != nil {
			return nil, fmt.Errorf("gapstate: list %s: %w", prefix, err)
		}
		if !strings.HasSuffix(info.Key, ".json") {
			continue
		}
		keys = append(keys, info.Key)
	}
	return keys, nil
}

// List walks every gap record under wal/<deployment>/gaps/ and
// returns them newest-first by DetectedAt. Cost: O(N) over the
// gap-record set per deployment. Practical fleets see at most
// a handful per failover; safe for `doctor`.
func (s *Store) List(ctx context.Context, deployment string) ([]Record, error) {
	if deployment == "" {
		return nil, errors.New("gapstate: empty deployment")
	}
	keyed, err := s.listKeyed(ctx, deployment)
	if err != nil {
		return nil, err
	}
	out := make([]Record, len(keyed))
	for i, kr := range keyed {
		out[i] = kr.rec
	}
	return out, nil
}

// Latest returns the newest gap record for (deployment, tli),
// or (zero, false) when none exist on the timeline. Used by
// `doctor`'s per-deployment surface.
func (s *Store) Latest(ctx context.Context, deployment string, tli uint32) (Record, bool, error) {
	all, err := s.List(ctx, deployment)
	if err != nil {
		return Record{}, false, err
	}
	for _, r := range all {
		if r.Timeline == tli {
			return r, true, nil
		}
	}
	return Record{}, false, nil
}

// LatestAny returns the newest gap record across ALL timelines
// for the deployment. Useful for the "any gap detected ever?"
// doctor headline.
func (s *Store) LatestAny(ctx context.Context, deployment string) (Record, bool, error) {
	all, err := s.List(ctx, deployment)
	if err != nil {
		return Record{}, false, err
	}
	if len(all) == 0 {
		return Record{}, false, nil
	}
	return all[0], true, nil
}

// PurgeOrphans removes gap records the deployment can no longer reach
// via any restore. Because recovery replays FORWARD and follows timeline
// switches, a backup on timeline S can cross a gap on every timeline
// >= S — so a gap is an orphan only when its timeline is strictly BELOW
// the lowest live-backup timeline. (It is NOT enough for the gap's own
// timeline to lack a live backup: a gap recorded on a newer timeline
// than the oldest live backup is still crossed by that backup's
// forward PITR.) The natural cleanup pass once all backups old enough to
// reach a gap have been retired.
//
// Returns the records that were removed (sorted newest-first
// per List's contract) so callers can audit-emit the cleanup +
// render the result.
//
// liveTimelines must be populated by the caller — this package
// has no visibility into the manifest set. The CLI wires it up
// from `ManifestStore.List` to keep the policy decision
// (which TLIs are live) in one place.
//
// dryRun=true walks + returns the would-be-removed set without
// mutating; same posture as repo gc / hold purge-expired. An
// empty liveTimelines is REJECTED (would treat every record as
// orphan and reap the whole tree); use PurgeAll for that.
func (s *Store) PurgeOrphans(ctx context.Context, deployment string, liveTimelines map[uint32]struct{}, dryRun bool) ([]Record, error) {
	if deployment == "" {
		return nil, errors.New("gapstate: empty deployment")
	}
	if liveTimelines == nil || len(liveTimelines) == 0 {
		return nil, errors.New("gapstate: PurgeOrphans requires a non-empty liveTimelines set; use PurgeAll for fleet-wipe semantics")
	}
	all, err := s.listKeyed(ctx, deployment)
	if err != nil {
		return nil, err
	}
	// A gap on timeline T is reachable by any live backup on T OR an
	// EARLIER timeline: recovery replays FORWARD and follows timeline
	// switches (recovery_target_timeline='latest'), so a backup on TLI S
	// can cross a gap on every TLI >= S. preflightWALGap checks the
	// target LSN against ALL gaps regardless of timeline, so the purge
	// must match that reach. Keep a gap iff its timeline >= the lowest
	// live-backup timeline; only gaps strictly below EVERY live backup
	// are unreachable by any forward PITR and safe to reap. (Keying off
	// exact membership wrongly purged a gap recorded on a newer timeline
	// than the live backups — e.g. a TLI-1 backup whose PITR follows
	// into the TLI-2 gap that the failover created.)
	var minLive uint32
	firstLive := true
	for tli := range liveTimelines {
		if firstLive || tli < minLive {
			minLive = tli
			firstLive = false
		}
	}
	var removed []Record
	for _, kr := range all {
		r := kr.rec
		if r.Timeline >= minLive {
			continue
		}
		if !dryRun {
			// Delete the exact key List read, not one rebuilt from
			// the body — a rebuilt key can miss the real object.
			if err := s.sp.Delete(ctx, kr.key); err != nil {
				// Mid-walk failure: return the partial
				// success list + the error. Same posture
				// as PurgeExpiredHolds and SoftDeleteCascade
				// — caller can re-run; the operation is
				// naturally idempotent (a Delete of an
				// already-gone key is a no-op on most
				// storage plugins).
				return removed, fmt.Errorf("gapstate: delete orphan %s: %w", kr.key, err)
			}
		}
		removed = append(removed, r)
	}
	return removed, nil
}

// PurgeAll removes every gap record for the named deployment.
// Used when a deployment is being wiped end-to-end (e.g., the
// operator removed it from config + ran cleanup); the routine
// orphan-purge path (PurgeOrphans) refuses to do this because
// an empty liveTimelines would over-reap.
//
// dryRun=true walks + returns the would-be-removed set without
// mutating.
func (s *Store) PurgeAll(ctx context.Context, deployment string, dryRun bool) ([]Record, error) {
	if deployment == "" {
		return nil, errors.New("gapstate: empty deployment")
	}
	// Wipe by raw key, not by parsed record: a corrupt or
	// schema-drifted .json under the prefix is still a gap file the
	// "end-to-end wipe" contract must remove. List would silently
	// skip it, leaving an orphan behind.
	keys, err := s.listRawKeys(ctx, deployment)
	if err != nil {
		return nil, err
	}
	if dryRun {
		// Preview: return the parseable records (best-effort audit
		// surface). The delete path below still covers every key.
		return s.List(ctx, deployment)
	}
	var removed []Record
	for _, key := range keys {
		// Best-effort parse for the audit trail before deleting;
		// an unparseable record is still removed.
		if rec, rerr := s.read(ctx, key); rerr == nil {
			removed = append(removed, rec)
		}
		if err := s.sp.Delete(ctx, key); err != nil {
			return removed, fmt.Errorf("gapstate: delete %s: %w", key, err)
		}
	}
	sort.SliceStable(removed, func(i, j int) bool {
		return removed[i].DetectedAt.After(removed[j].DetectedAt)
	})
	return removed, nil
}

func (s *Store) read(ctx context.Context, key string) (Record, error) {
	rc, err := s.sp.Get(ctx, key)
	if err != nil {
		return Record{}, err
	}
	defer rc.Close()
	body, err := stdio.ReadAll(rc)
	if err != nil {
		return Record{}, fmt.Errorf("read body: %w", err)
	}
	var r Record
	if err := json.Unmarshal(body, &r); err != nil {
		return Record{}, fmt.Errorf("decode: %w", err)
	}
	if r.Schema != Schema {
		return Record{}, fmt.Errorf("unexpected schema %q (want %q)", r.Schema, Schema)
	}
	return r, nil
}

// prefixFor returns the wal/<deployment>/gaps/ prefix.
func prefixFor(deployment string) string {
	return "wal/" + deployment + "/gaps/"
}

// keyFor derives the record's canonical storage key. The key
// embeds the TLI + unix-nano so List ordering is meaningful
// even before we parse bodies.
func keyFor(deployment string, tli uint32, detectedAt time.Time) string {
	nanos := detectedAt.UTC().UnixNano()
	return prefixFor(deployment) + strconv.FormatUint(uint64(tli), 10) + "-" + strconv.FormatInt(nanos, 10) + ".json"
}
