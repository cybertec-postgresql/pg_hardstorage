// walprune.go — WALPrune: deletes WAL segments older than the cutoff LSN with bounded failure list.
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdio "io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// WALPruneSchema is the on-disk version tag for WALPruneResult bodies.
const WALPruneSchema = "pg_hardstorage.repo.wal_prune.v1"

// maxWALPruneFailures bounds the per-result Failures slice. Same
// posture as Replicate / Heal — counter totals stay unbounded; only
// per-key error detail is capped.
const maxWALPruneFailures = 50

// WALPruneOptions configures one WAL retention pass.
//
// Architectural note: this primitive lives at the storage layer
// (internal/repo) and operates via partial-decode of manifest
// bodies — same posture as repo.CollectReferences in gc.go. We
// don't take a *backup.Verifier here because that would create an
// import cycle (backup → repo → backup). Signature verification
// is the restore path's responsibility; storage-layer pruning
// trusts what's on disk the same way GC does.
type WALPruneOptions struct {
	// Deployment is the deployment whose WAL is being pruned.
	// Required — WAL is per-deployment in the repo layout.
	Deployment string

	// DryRun reports the candidates without deleting. Default
	// behavior; callers that want to actually mutate flip this off.
	DryRun bool

	// KeepFloorTime, when non-zero, is a hard "do not delete WAL
	// segments newer than this" floor. Even segments whose end_lsn
	// is below the oldest-kept-backup frontier are preserved when
	// their CreatedAt is past the floor. Operators set this to
	// `time.Now() - keep_wal_days` to enforce the plan's
	// "WAL retained for PITR window" semantics on top of the
	// LSN-based primary rule.
	KeepFloorTime time.Time

	// TombstoneGrace mirrors repo gc's defence (gc.go, audit):
	// a backup whose tombstone marker is YOUNGER than this still counts
	// toward the WAL frontier, so the WAL it needs survives and a
	// `backup undelete` inside the window can still recover it. Without
	// this, prune would delete a freshly-tombstoned backup's WAL even
	// though gc is still preserving its chunks for undelete — leaving an
	// undeleted backup unrecoverable. Zero → DefaultTombstoneGracePeriod;
	// negative → no grace (every tombstone excluded immediately).
	TombstoneGrace time.Duration

	// Now is the clock used for the tombstone-grace comparison. Zero
	// means time.Now().UTC(); tests inject a fixed instant.
	Now time.Time

	// OnProgress fires per WAL segment as it's classified. Optional.
	OnProgress func(ev WALPruneProgress)
}

// WALPruneProgress is the per-segment callback shape.
type WALPruneProgress struct {
	SegmentName string
	Outcome     string // "kept" | "would_delete" | "deleted" | "failed"
	Reason      string
}

// WALPruneFailure records one segment that couldn't be deleted.
type WALPruneFailure struct {
	Key string `json:"key"`
	Err string `json:"err"`
}

// WALPruneResult is the structured outcome.
type WALPruneResult struct {
	Schema     string    `json:"schema"`
	StartedAt  time.Time `json:"started_at"`
	StoppedAt  time.Time `json:"stopped_at"`
	DurationMS int64     `json:"duration_ms"`

	Deployment string `json:"deployment"`
	DryRun     bool   `json:"dry_run"`

	// FrontierLSN is the start_lsn of the oldest non-tombstoned
	// backup. WAL segments whose end_lsn is < this LSN are
	// candidates for deletion. Empty when no backup exists yet
	// (no frontier → conservative: nothing pruned).
	FrontierLSN      string `json:"frontier_lsn,omitempty"`
	FrontierBackupID string `json:"frontier_backup_id,omitempty"`

	// KeepFloor reflects the configured floor (RFC3339); empty
	// when no floor was set.
	KeepFloor string `json:"keep_floor,omitempty"`

	SegmentsConsidered  int   `json:"segments_considered"`
	SegmentsDeleted     int   `json:"segments_deleted"`
	SegmentsKept        int   `json:"segments_kept"`
	SegmentsKeptByFloor int   `json:"segments_kept_by_floor,omitempty"`
	SegmentsFailed      int   `json:"segments_failed"`
	BytesDeleted        int64 `json:"bytes_deleted"`

	Failures []WALPruneFailure `json:"failures,omitempty"`
}

// WALPrune deletes WAL segment manifests whose end_lsn is strictly
// older than the frontier — the start_lsn of the oldest
// non-tombstoned backup for the deployment. Segments at or after
// the frontier are kept (they're needed for PITR).
//
// Why end_lsn < frontier (strict)? PITR replays WAL forward from a
// base backup's stop_lsn (or start_lsn for non-exclusive backups
// in PG 15+). A segment whose final byte's LSN is < the frontier
// can never participate in a recovery that uses any kept backup,
// so keeping it serves no purpose.
//
// What WALPrune does NOT do:
//
//   - Touch chunks. The WAL segment manifests reference chunks in
//     the CAS; deleting a manifest leaves its chunks as orphans
//     for the next `repo gc` pass to sweep. This is the same split
//     `rotate` uses for backup manifests: prune the references,
//     let GC handle the bytes.
//
//   - Touch timeline-history files (wal/<dep>/timelines/<tli>.history).
//     Those are tiny and cross-version-critical — pruning them is
//     never safe.
//
//   - Apply when no backup exists. Without a frontier we can't
//     know what to keep, so the conservative answer is "keep
//     everything." (Operators with a fresh repo who want to clear
//     pre-bootstrap WAL can still use `repo gc` directly.)
//
// The function is goroutine-safe per StoragePlugin contract; it
// holds no shared state of its own.
func WALPrune(ctx context.Context, sp storage.StoragePlugin, opts WALPruneOptions) (*WALPruneResult, error) {
	if sp == nil {
		return nil, errors.New("repo wal prune: nil StoragePlugin")
	}
	if opts.Deployment == "" {
		return nil, errors.New("repo wal prune: Deployment is required")
	}
	res := &WALPruneResult{
		Schema:     WALPruneSchema,
		StartedAt:  time.Now().UTC(),
		Deployment: opts.Deployment,
		DryRun:     opts.DryRun,
	}
	if !opts.KeepFloorTime.IsZero() {
		res.KeepFloor = opts.KeepFloorTime.UTC().Format(time.RFC3339)
	}
	finish := func() {
		res.StoppedAt = time.Now().UTC()
		res.DurationMS = res.StoppedAt.Sub(res.StartedAt).Milliseconds()
	}

	// 1. Find the frontier: oldest non-tombstoned backup for the
	//    deployment + its start_lsn. We walk the manifests/<dep>/
	//    prefix directly (partial-decode + tombstone-skip) to
	//    avoid a backup-package import cycle.
	// A tombstone OLDER than this cutoff excludes its backup from the
	// frontier (its WAL becomes prunable); a YOUNGER one keeps it, so an
	// Undelete within the grace window still has the WAL it needs. Same
	// grace semantics as repo gc's chunk retention.
	grace := walPruneGrace(opts)
	tombstoneCutoff := walPruneNow(opts).Add(-grace)
	frontierLSN, frontierBackupID, err := oldestKeptBackupFrontier(ctx, sp, opts.Deployment, tombstoneCutoff, grace > 0)
	if err != nil {
		finish()
		return res, fmt.Errorf("repo wal prune: find frontier: %w", err)
	}
	if frontierLSN == 0 && frontierBackupID == "" {
		// No backups yet — nothing to prune. Honest no-op.
		finish()
		return res, nil
	}
	res.FrontierLSN = frontierLSN.String()
	res.FrontierBackupID = frontierBackupID

	// 2. Walk WAL segment manifests under wal/<dep>/<TLI>/*.json.
	keys, err := listWALSegmentKeys(ctx, sp, opts.Deployment)
	if err != nil {
		finish()
		return res, fmt.Errorf("repo wal prune: list segments: %w", err)
	}

	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			finish()
			return res, err
		}
		res.SegmentsConsidered++

		segName := segmentNameFromKey(key)

		// Read the segment manifest to get its end_lsn + CreatedAt.
		mani, err := readSegmentManifest(ctx, sp, key)
		if err != nil {
			recordWALPruneFailure(res, key, fmt.Errorf("read segment manifest: %w", err))
			res.SegmentsFailed++
			emitWALPruneProgress(opts, segName, "failed", err.Error())
			continue
		}

		segEnd, err := pglogrepl.ParseLSN(mani.EndLSN)
		if err != nil {
			recordWALPruneFailure(res, key, fmt.Errorf("parse end_lsn %q: %w", mani.EndLSN, err))
			res.SegmentsFailed++
			emitWALPruneProgress(opts, segName, "failed", err.Error())
			continue
		}

		// Primary rule: end_lsn < frontier → candidate.
		if segEnd >= frontierLSN {
			res.SegmentsKept++
			emitWALPruneProgress(opts, segName, "kept",
				fmt.Sprintf("end_lsn %s >= frontier %s", segEnd, frontierLSN))
			continue
		}

		// Time-floor: if KeepFloorTime is set and the segment's
		// CreatedAt is at-or-after the floor, keep it.
		if !opts.KeepFloorTime.IsZero() && !mani.CreatedAt.IsZero() &&
			!mani.CreatedAt.Before(opts.KeepFloorTime) {
			res.SegmentsKept++
			res.SegmentsKeptByFloor++
			emitWALPruneProgress(opts, segName, "kept",
				fmt.Sprintf("CreatedAt %s >= keep-floor %s",
					mani.CreatedAt.Format(time.RFC3339), opts.KeepFloorTime.Format(time.RFC3339)))
			continue
		}

		if opts.DryRun {
			res.SegmentsDeleted++
			res.BytesDeleted += sumChunkLengths(mani.Chunks)
			emitWALPruneProgress(opts, segName, "would_delete",
				fmt.Sprintf("end_lsn %s < frontier %s", segEnd, frontierLSN))
			continue
		}

		// Real delete — manifest only; chunks are GC's job.
		if err := sp.Delete(ctx, key); err != nil {
			recordWALPruneFailure(res, key, fmt.Errorf("delete: %w", err))
			res.SegmentsFailed++
			emitWALPruneProgress(opts, segName, "failed", err.Error())
			continue
		}
		res.SegmentsDeleted++
		res.BytesDeleted += sumChunkLengths(mani.Chunks)
		emitWALPruneProgress(opts, segName, "deleted", "")
	}

	finish()
	return res, nil
}

// backupManifestDecode is a partial-decode shape for what
// WALPrune needs from a backup manifest: backup_id (for the
// frontier-source label) and start_lsn (the frontier value — the
// minimum across kept backups is the prune floor). Mirrors the
// walSegmentDecode pattern.
type backupManifestDecode struct {
	BackupID string `json:"backup_id"`
	StartLSN string `json:"start_lsn"`
}

// walPruneGrace / walPruneNow resolve the tombstone-grace knobs, mirroring
// gc.CollectReferencesOptions: zero grace → the shared default, negative →
// none; zero Now → wallclock.
func walPruneGrace(o WALPruneOptions) time.Duration {
	if o.TombstoneGrace == 0 {
		return DefaultTombstoneGracePeriod
	}
	if o.TombstoneGrace < 0 {
		return 0
	}
	return o.TombstoneGrace
}

func walPruneNow(o WALPruneOptions) time.Time {
	if o.Now.IsZero() {
		return time.Now().UTC()
	}
	return o.Now.UTC()
}

// oldestKeptBackupFrontier walks the manifests/<dep>/backups/ prefix,
// skips tombstoned IDs, and returns the MINIMUM start_lsn across all
// committed backups — the earliest WAL position any kept backup needs
// to reach consistency / serve PITR. Returns (0, "", nil) when no
// backup exists. Uses partial-decode (no signature verification) to
// avoid a backup-package import cycle — same posture as
// gc.CollectReferences.
//
// The frontier is min(start_lsn), NOT the start_lsn of the
// oldest-by-StoppedAt backup. Those differ whenever a backup started
// at an earlier LSN but finished LATER (a long-running backup that
// overlaps a quick one, concurrent non-exclusive backups, or
// wall-clock skew reordering StoppedAt vs LSN). Keying off StoppedAt
// there would set the frontier too high and prune WAL segments the
// earlier-LSN backup still needs — silently making it unrestorable.
//
// A tombstoned backup is excluded ONLY when its tombstone marker is older
// than tombstoneCutoff (now - grace). A younger tombstone still counts, so
// its WAL survives for a `backup undelete` within the grace window — the same
// defence repo gc applies to the chunks (gc.go, audit). Without it,
// prune would strand an undeleted backup with no WAL.
func oldestKeptBackupFrontier(ctx context.Context, sp storage.StoragePlugin, deployment string, tombstoneCutoff time.Time, graceActive bool) (pglogrepl.LSN, string, error) {
	prefix := "manifests/" + deployment + "/backups/"
	tombstoned := map[string]struct{}{}
	var manifestKeys []string
	for info, err := range sp.List(ctx, prefix) {
		if err != nil {
			return 0, "", fmt.Errorf("list manifests: %w", err)
		}
		key := info.Key
		switch {
		case strings.HasSuffix(key, "/manifest.json.tombstone"):
			// Layout: manifests/<dep>/backups/<id>/manifest.json.tombstone
			// Only an OLD tombstone (marker older than the grace cutoff)
			// excludes its backup from the frontier. A young one keeps the
			// backup — and thus its WAL — recoverable via undelete, matching
			// repo gc's chunk-retention grace.
			//
			// A zero ModTime means the backend didn't expose the tombstone's
			// age in List (S3/azblob when the SDK omits LastModified) — and
			// the zero time sorts Before every real cutoff. Treating that as
			// an OLD tombstone would prune the WAL of a backup whose chunks
			// gc is still preserving (gc.go treats the same zero-ModTime case
			// as YOUNG, audit), stranding an undeleted backup with
			// chunks but no WAL. Mirror gc exactly: while grace is active,
			// an unknown age is YOUNG (keep the WAL); only when grace is
			// disabled (TombstoneGrace<0) does an unknown age count as old.
			old := info.ModTime.Before(tombstoneCutoff)
			if info.ModTime.IsZero() {
				old = !graceActive
			}
			if old {
				parts := strings.Split(strings.TrimPrefix(key, prefix), "/")
				if len(parts) > 0 {
					tombstoned[parts[0]] = struct{}{}
				}
			}
		case strings.HasSuffix(key, "/manifest.json"):
			if strings.Contains(key, ".tmp.") {
				continue
			}
			manifestKeys = append(manifestKeys, key)
		}
	}

	var (
		minLSN      pglogrepl.LSN
		minBackupID string
		found       bool
	)
	for _, key := range manifestKeys {
		// Extract the backup ID from the key shape and skip
		// tombstoned ones.
		rel := strings.TrimPrefix(key, prefix)
		slash := strings.IndexByte(rel, '/')
		if slash <= 0 {
			continue
		}
		backupID := rel[:slash]
		if _, dead := tombstoned[backupID]; dead {
			continue
		}
		body, err := readKey(ctx, sp, key)
		if err != nil {
			// Fail closed. This is a LIVE (non-tombstoned) backup
			// whose manifest we cannot read, so we cannot learn its
			// start_lsn — the same blind spot as a malformed
			// start_lsn below. Silently skipping it would drop it
			// from the min(start_lsn) frontier, letting the frontier
			// advance past a backup that still needs older WAL and
			// prune that WAL away: a silent, unrecoverable PITR gap.
			// Refuse to prune until the operator heals the manifest
			// (surfaced by `doctor`).
			return 0, "", fmt.Errorf("read manifest %s for WAL frontier: %w", key, err)
		}
		var m backupManifestDecode
		if err := json.Unmarshal(body, &m); err != nil {
			// Same rationale as the unreadable case: a corrupt
			// manifest envelope hides start_lsn, so fail closed
			// rather than risk pruning WAL the backup depends on.
			return 0, "", fmt.Errorf("decode manifest %s for WAL frontier: %w", key, err)
		}
		// Parse every backup's start_lsn so we can take the true
		// minimum. A malformed start_lsn is fail-closed (abort the
		// prune): we cannot know how far back that backup needs WAL, so
		// pruning anything would risk deleting WAL it depends on.
		lsn, err := pglogrepl.ParseLSN(m.StartLSN)
		if err != nil {
			return 0, "", fmt.Errorf("parse start_lsn %q of backup %s: %w",
				m.StartLSN, m.BackupID, err)
		}
		if !found || lsn < minLSN {
			minLSN = lsn
			minBackupID = m.BackupID
			found = true
		}
	}
	if !found {
		return 0, "", nil
	}
	return minLSN, minBackupID, nil
}

// listWALSegmentKeys enumerates wal/<deployment>/<tli>/<seg>.json
// keys. Sorted lexicographically (which matches WAL chronological
// order when timeline + segment numbers monotonically increase, the
// normal case). Skips .tmp and timeline history files.
func listWALSegmentKeys(ctx context.Context, sp storage.StoragePlugin, deployment string) ([]string, error) {
	prefix := "wal/" + deployment + "/"
	var out []string
	for info, err := range sp.List(ctx, prefix) {
		if err != nil {
			return nil, err
		}
		key := info.Key
		if !strings.HasSuffix(key, ".json") {
			continue
		}
		if strings.Contains(key, ".json.tmp.") {
			continue
		}
		// Skip timeline history files (wal/<dep>/timelines/<tli>.history)
		// — they're not segment manifests.
		if strings.Contains(key, "/timelines/") {
			continue
		}
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}

// walSegmentDecode is a partial-decode shape for what WALPrune
// needs from a segment manifest: end_lsn (LSN bound check),
// created_at (time-floor check), chunks (BytesDeleted accounting).
//
// We deliberately don't import walsink.SegmentManifest here —
// that would create an import cycle (walsink → repo → walsink).
// gc.go uses the same partial-decode pattern; consistent posture.
type walSegmentDecode struct {
	EndLSN    string    `json:"end_lsn"`
	CreatedAt time.Time `json:"created_at"`
	Chunks    []struct {
		Len int64 `json:"len"`
	} `json:"chunks"`
}

// readSegmentManifest fetches and partial-decodes a single WAL
// segment manifest at key.
func readSegmentManifest(ctx context.Context, sp storage.StoragePlugin, key string) (*walSegmentDecode, error) {
	rc, err := sp.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := stdio.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	var m walSegmentDecode
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("decode segment manifest: %w", err)
	}
	return &m, nil
}

// segmentNameFromKey extracts the 24-char segment name from a key
// of the form wal/<dep>/<TLI>/<24-char>.json.
func segmentNameFromKey(key string) string {
	if !strings.HasSuffix(key, ".json") {
		return key
	}
	base := strings.TrimSuffix(key, ".json")
	if i := strings.LastIndex(base, "/"); i >= 0 {
		return base[i+1:]
	}
	return base
}

// sumChunkLengths sums the Len of a chunk-ref list (from the
// partial-decode shape). Used for the BytesDeleted counter —
// it's the LOGICAL bytes the deleted manifest referenced (not
// the on-disk chunk size, which is post-compression /
// post-encryption envelope).
func sumChunkLengths(chunks []struct {
	Len int64 `json:"len"`
}) int64 {
	var total int64
	for _, c := range chunks {
		total += c.Len
	}
	return total
}

func emitWALPruneProgress(opts WALPruneOptions, segName, outcome, reason string) {
	if opts.OnProgress == nil {
		return
	}
	opts.OnProgress(WALPruneProgress{
		SegmentName: segName,
		Outcome:     outcome,
		Reason:      reason,
	})
}

func recordWALPruneFailure(res *WALPruneResult, key string, err error) {
	if len(res.Failures) >= maxWALPruneFailures {
		return
	}
	res.Failures = append(res.Failures, WALPruneFailure{
		Key: key,
		Err: err.Error(),
	})
}
