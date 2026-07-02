// replicate_verify.go — read-only consistency check between a
// primary repo and a replica, emitting a consistent/drifted/broken
// verdict plus a bounded per-key failure list.

package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// jsonUnmarshalImpl is the canonical unmarshal entry point for
// this file.  Wrapped so a future migration to a non-JSON
// manifest format doesn't ripple through every caller.
func jsonUnmarshalImpl(body []byte, into any) error {
	return json.Unmarshal(body, into)
}

// ReplicateVerifySchema is the on-disk version tag for
// ReplicateVerifyResult bodies.  Stable per the v1 commitment.
const ReplicateVerifySchema = "pg_hardstorage.repo.replicate_verify.v1"

// maxReplicateVerifyFailures caps the per-result Failures slice
// (same posture as Replicate / Heal / RotateKEK / VerifyEnvelopes).
// Counter totals stay unbounded; only per-key error detail is
// capped so a fleet with thousands of drifted keys doesn't blow
// out the JSON body.
const maxReplicateVerifyFailures = 200

// ReplicateVerifyVerdict is the overall verdict.  Three values:
//
//   - consistent — every primary key is present at the replica
//     with matching content (size + on-disk ETag if available).
//   - drifted    — one or more keys is present but with mismatched
//     content; the replica is desynchronized.
//   - broken     — one or more keys is absent from the replica;
//     the replica can't serve those backups at all.
//
// Operators distinguish drifted from broken because the
// remediation differs: drifted needs a re-replicate of the
// affected keys; broken needs a re-replicate AND an investigation
// into why the original replicate didn't write them (transient
// 503, replicate worker stopped early, etc.).
type ReplicateVerifyVerdict string

const (
	// VerdictConsistent reports that every primary key is present
	// at the replica with matching content (size + on-disk ETag
	// when available, or full hash compare in deep mode).
	VerdictConsistent ReplicateVerifyVerdict = "consistent"

	// VerdictDrifted reports that one or more keys are present at
	// the replica but with mismatched content. Remediation: re-
	// replicate the affected keys.
	VerdictDrifted ReplicateVerifyVerdict = "drifted"

	// VerdictBroken reports that one or more keys are absent from
	// the replica. Remediation: re-replicate AND investigate why
	// the original replicate didn't write them.
	VerdictBroken ReplicateVerifyVerdict = "broken"
)

// ReplicateVerifyOptions configures a verification pass.
//
// Walk costs:
//
//   - Default (Stat-only): O(N keys) Stat calls against the
//     replica.  Fast.  Detects "missing" but not "drifted" via
//     content; relies on size+ETag-from-Stat for content drift.
//   - Deep (--deep): O(N keys) full Gets against both ends.
//     Hashes both sides + compares.  Slow.  Suitable for periodic
//     deep scrub; not for per-PR / per-deploy checks.
//
// Read-only against both repos.  Safe at any cadence including
// against WORM-locked replicas.
type ReplicateVerifyOptions struct {
	// Deployment, when non-empty, restricts the walk to one
	// deployment's manifest tree.  Repo-level objects (HSREPO,
	// audit chain, approvals) are NOT included regardless —
	// those are out of scope for replicate-verify; the
	// `repo audit` surface is the right place for repo-wide
	// inventory.
	Deployment string

	// IncludeWAL, when true, also verifies wal/<deployment>/...
	// segment manifests + WAL chunks.  Default false: the
	// hot path is backup-level redundancy.
	IncludeWAL bool

	// Deep, when true, fetches both sides + compares content
	// (post-envelope: encrypted chunks have to round-trip the
	// envelope to verify, which the Stat-only path skips).
	// Default false: Stat-only.
	Deep bool

	// SampleRate, when between 0.0 and 1.0, restricts the deep
	// check to a proportion of the considered keys.  Pure-Stat
	// passes consider every key regardless.  Default 1.0
	// (every key) when Deep is true.
	SampleRate float64

	// OnProgress fires per considered key.  Optional;
	// synchronous.
	OnProgress func(ev ReplicateVerifyProgress)
}

// ReplicateVerifyProgress is the per-step callback shape.
type ReplicateVerifyProgress struct {
	Stage   string // "manifest" | "chunk" | "wal_manifest"
	Current string // the source key being checked
	Status  string // "present" | "missing" | "content_drift"
}

// ReplicateVerifyFailure records one key that's missing or
// drifted at the replica.
type ReplicateVerifyFailure struct {
	Kind     string `json:"kind"` // "missing_manifest" | "missing_chunk" | "missing_wal_manifest" | "content_drift"
	Key      string `json:"key"`
	BackupID string `json:"backup_id,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// ReplicateVerifyResult is the structured outcome.
type ReplicateVerifyResult struct {
	Schema     string    `json:"schema"`
	StartedAt  time.Time `json:"started_at"`
	StoppedAt  time.Time `json:"stopped_at"`
	DurationMS int64     `json:"duration_ms"`

	SourceURL string `json:"source_url,omitempty"`
	DestURL   string `json:"dest_url,omitempty"`

	Deployment string `json:"deployment,omitempty"`
	IncludeWAL bool   `json:"include_wal,omitempty"`
	Deep       bool   `json:"deep,omitempty"`

	ManifestsConsidered   int `json:"manifests_considered"`
	ManifestsPresent      int `json:"manifests_present"`
	ManifestsMissing      int `json:"manifests_missing,omitempty"`
	ManifestsContentDrift int `json:"manifests_content_drift,omitempty"`
	// ManifestsTombstoned counts src manifests skipped because the
	// backup is soft-deleted (tombstoned). Replicate deliberately
	// never copies these, so verify must skip them too — otherwise a
	// healthy replica is judged "broken" for correctly lacking them.
	ManifestsTombstoned int `json:"manifests_tombstoned,omitempty"`

	ChunksConsidered   int `json:"chunks_considered"`
	ChunksPresent      int `json:"chunks_present"`
	ChunksMissing      int `json:"chunks_missing,omitempty"`
	ChunksContentDrift int `json:"chunks_content_drift,omitempty"`

	WALManifestsConsidered int `json:"wal_manifests_considered,omitempty"`
	WALManifestsPresent    int `json:"wal_manifests_present,omitempty"`
	WALManifestsMissing    int `json:"wal_manifests_missing,omitempty"`

	// WAL auxiliary files (.history/.backup/.partial): direct-byte files
	// the DR repo needs for cross-failover PITR.
	WALAuxConsidered int `json:"wal_aux_considered,omitempty"`
	WALAuxPresent    int `json:"wal_aux_present,omitempty"`
	WALAuxMissing    int `json:"wal_aux_missing,omitempty"`

	Failures []ReplicateVerifyFailure `json:"failures,omitempty"`

	Verdict ReplicateVerifyVerdict `json:"verdict"`
}

// AnyMissing reports whether any key was absent at the replica.
func (r *ReplicateVerifyResult) AnyMissing() bool {
	return r.ManifestsMissing+r.ChunksMissing+r.WALManifestsMissing+r.WALAuxMissing > 0
}

// AnyDrifted reports whether any present key had drifted content.
func (r *ReplicateVerifyResult) AnyDrifted() bool {
	return r.ManifestsContentDrift+r.ChunksContentDrift > 0
}

// VerifyReplicate walks src + dst and reports the verification
// verdict.  Both src and dst must be ALREADY-OPEN StoragePlugins;
// caller owns Close.
//
// The walk shape mirrors Replicate's: we enumerate src's manifest
// store, fetch each manifest body, and verify both the manifest
// itself + its chunk references + (when IncludeWAL) WAL segment
// manifests + WAL chunks.
//
// We do NOT verify a chunk that's referenced by multiple manifests
// twice — the considered/present counters dedupe by hash.
func VerifyReplicate(ctx context.Context, src, dst storage.StoragePlugin, opts ReplicateVerifyOptions) (*ReplicateVerifyResult, error) {
	if src == nil {
		return nil, errors.New("repo: VerifyReplicate: nil src StoragePlugin")
	}
	if dst == nil {
		return nil, errors.New("repo: VerifyReplicate: nil dst StoragePlugin")
	}
	if opts.SampleRate < 0 || opts.SampleRate > 1 {
		return nil, fmt.Errorf("repo: VerifyReplicate: SampleRate must be in [0,1]; got %v", opts.SampleRate)
	}
	if opts.SampleRate == 0 {
		opts.SampleRate = 1.0
	}

	res := &ReplicateVerifyResult{
		Schema:     ReplicateVerifySchema,
		StartedAt:  time.Now().UTC(),
		Deployment: opts.Deployment,
		IncludeWAL: opts.IncludeWAL,
		Deep:       opts.Deep,
	}
	finish := func() {
		res.StoppedAt = time.Now().UTC()
		res.DurationMS = res.StoppedAt.Sub(res.StartedAt).Milliseconds()
		res.Verdict = computeReplicateVerdict(res)
	}

	// We walk source manifests directly via a List of the
	// manifest tree, scoped to the deployment when set.  This
	// mirrors what Replicate does internally so the verify pass
	// covers the same set of objects the writer would.
	prefix := "manifests/"
	if opts.Deployment != "" {
		prefix = "manifests/" + opts.Deployment + "/backups/"
	}

	// Collect tombstoned backup IDs at src first (mirrors Replicate's
	// Pass 1). Tombstoned backups are soft-deleted; Replicate never
	// copies them, so their manifests/chunks legitimately do not exist
	// at the replica. Verifying them would flag a healthy replica as
	// broken. A separate pass keeps this O(N) without a
	// list-during-iteration footgun.
	tombstoned := map[string]struct{}{}
	for info, lerr := range src.List(ctx, prefix) {
		if err := ctx.Err(); err != nil {
			finish()
			return res, err
		}
		if lerr != nil {
			finish()
			return res, fmt.Errorf("repo: VerifyReplicate: list src %q: %w", prefix, lerr)
		}
		if !strings.HasSuffix(info.Key, "/manifest.json.tombstone") {
			continue
		}
		// Layout: manifests/<dep>/backups/<id>/manifest.json.tombstone
		parts := strings.Split(info.Key, "/")
		if len(parts) >= 4 {
			tombstoned[parts[3]] = struct{}{}
		}
	}

	// Pass 1: walk manifest keys at src.  For each manifest:
	//   - confirm the same key exists at dst (Stat-only by default)
	//   - parse the manifest body to enumerate chunk hashes
	//   - confirm each chunk exists at dst
	chunksSeen := map[string]struct{}{}

	for info, lerr := range src.List(ctx, prefix) {
		if err := ctx.Err(); err != nil {
			finish()
			return res, err
		}
		if lerr != nil {
			finish()
			return res, fmt.Errorf("repo: VerifyReplicate: list src %q: %w", prefix, lerr)
		}
		// Skip non-manifest keys (tombstones, holds, etc.) +
		// the _replicas mirror tree (we're verifying the
		// PRIMARY → REPLICA copy; not the in-repo replica
		// metadata).
		if !isManifestKey(info.Key) {
			continue
		}
		// Skip tombstoned (soft-deleted) backups: Replicate never
		// copies them, so a healthy replica correctly lacks them.
		if id := backupIDFromKey(info.Key); id != "" {
			if _, dead := tombstoned[id]; dead {
				res.ManifestsTombstoned++
				continue
			}
		}
		res.ManifestsConsidered++

		// Manifest presence + content check.
		drift, err := verifyKey(ctx, src, dst, info.Key, info.Size, opts)
		if err != nil {
			if !errors.Is(err, errReplicaKeyMissing) {
				// Transient/backend or src-side error — we cannot
				// conclude the replica is broken. Abort the verify
				// and propagate rather than falsely flagging missing.
				finish()
				return res, fmt.Errorf("repo: VerifyReplicate: verify manifest %q: %w", info.Key, err)
			}
			recordReplicateVerifyFailure(res, ReplicateVerifyFailure{
				Kind:   "missing_manifest",
				Key:    info.Key,
				Reason: err.Error(),
			})
			res.ManifestsMissing++
			emitReplicateVerifyProgress(opts, "manifest", info.Key, "missing")
			continue
		}
		if drift {
			recordReplicateVerifyFailure(res, ReplicateVerifyFailure{
				Kind:   "content_drift",
				Key:    info.Key,
				Reason: "size mismatch between primary and replica",
			})
			res.ManifestsContentDrift++
			emitReplicateVerifyProgress(opts, "manifest", info.Key, "content_drift")
		} else {
			res.ManifestsPresent++
			emitReplicateVerifyProgress(opts, "manifest", info.Key, "present")
		}

		// Walk chunk references.  We need the manifest body to
		// enumerate chunk hashes; there's no shortcut.
		chunkKeys, manifestParseErr := chunkKeysFromManifest(ctx, src, info.Key)
		if manifestParseErr != nil {
			// A torn manifest at src is a primary-side concern;
			// surface as a content-drift failure on the manifest
			// (we already counted it above) and skip the chunk
			// walk for this manifest.
			continue
		}
		for _, chunkKey := range chunkKeys {
			if err := ctx.Err(); err != nil {
				finish()
				return res, err
			}
			if _, seen := chunksSeen[chunkKey]; seen {
				continue
			}
			chunksSeen[chunkKey] = struct{}{}
			res.ChunksConsidered++
			drift, err := verifyKey(ctx, src, dst, chunkKey, 0, opts)
			if err != nil {
				if !errors.Is(err, errReplicaKeyMissing) {
					// Transient/backend or src-side error — abort
					// rather than falsely flagging the chunk missing.
					finish()
					return res, fmt.Errorf("repo: VerifyReplicate: verify chunk %q: %w", chunkKey, err)
				}
				recordReplicateVerifyFailure(res, ReplicateVerifyFailure{
					Kind:     "missing_chunk",
					Key:      chunkKey,
					BackupID: backupIDFromKey(info.Key),
					Reason:   err.Error(),
				})
				res.ChunksMissing++
				emitReplicateVerifyProgress(opts, "chunk", chunkKey, "missing")
				continue
			}
			if drift {
				recordReplicateVerifyFailure(res, ReplicateVerifyFailure{
					Kind:     "content_drift",
					Key:      chunkKey,
					BackupID: backupIDFromKey(info.Key),
					Reason:   "size mismatch",
				})
				res.ChunksContentDrift++
				emitReplicateVerifyProgress(opts, "chunk", chunkKey, "content_drift")
			} else {
				res.ChunksPresent++
				emitReplicateVerifyProgress(opts, "chunk", chunkKey, "present")
			}
		}
	}

	// WAL pass — opt-in.
	if opts.IncludeWAL {
		walPrefix := "wal/"
		if opts.Deployment != "" {
			walPrefix = "wal/" + opts.Deployment + "/"
		}
		for info, lerr := range src.List(ctx, walPrefix) {
			if err := ctx.Err(); err != nil {
				finish()
				return res, err
			}
			if lerr != nil {
				finish()
				return res, fmt.Errorf("repo: VerifyReplicate: list wal %q: %w", walPrefix, lerr)
			}
			// WAL auxiliary files (.history/.backup/.partial) must also
			// be present at the replica — without the timeline .history a
			// DR restore can't navigate a failover. Verify their presence.
			if isWALAuxKey(info.Key) {
				res.WALAuxConsidered++
				if _, aerr := verifyKey(ctx, src, dst, info.Key, info.Size, opts); aerr != nil {
					if !errors.Is(aerr, errReplicaKeyMissing) {
						finish()
						return res, fmt.Errorf("repo: VerifyReplicate: verify wal aux %q: %w", info.Key, aerr)
					}
					recordReplicateVerifyFailure(res, ReplicateVerifyFailure{
						Kind:   "missing_wal_aux",
						Key:    info.Key,
						Reason: aerr.Error(),
					})
					res.WALAuxMissing++
					emitReplicateVerifyProgress(opts, "wal_aux", info.Key, "missing")
				} else {
					res.WALAuxPresent++
					emitReplicateVerifyProgress(opts, "wal_aux", info.Key, "present")
				}
				continue
			}
			if !isWALSegmentManifestKey(info.Key) {
				continue
			}
			res.WALManifestsConsidered++
			drift, err := verifyKey(ctx, src, dst, info.Key, info.Size, opts)
			if err != nil {
				if !errors.Is(err, errReplicaKeyMissing) {
					finish()
					return res, fmt.Errorf("repo: VerifyReplicate: verify wal manifest %q: %w", info.Key, err)
				}
				recordReplicateVerifyFailure(res, ReplicateVerifyFailure{
					Kind:   "missing_wal_manifest",
					Key:    info.Key,
					Reason: err.Error(),
				})
				res.WALManifestsMissing++
				emitReplicateVerifyProgress(opts, "wal_manifest", info.Key, "missing")
				continue
			}
			if drift {
				// WAL drift gets the chunk-style content-drift
				// classification.  The replica has the segment
				// manifest but with mismatched bytes — treat
				// like any other drift.
				recordReplicateVerifyFailure(res, ReplicateVerifyFailure{
					Kind:   "content_drift",
					Key:    info.Key,
					Reason: "WAL segment manifest size mismatch",
				})
				res.ChunksContentDrift++
				emitReplicateVerifyProgress(opts, "wal_manifest", info.Key, "content_drift")
			} else {
				res.WALManifestsPresent++
				emitReplicateVerifyProgress(opts, "wal_manifest", info.Key, "present")
			}
		}
	}

	finish()
	return res, nil
}

// computeReplicateVerdict rolls Result counters into a verdict.
// Order of precedence: broken > drifted > consistent.
func computeReplicateVerdict(r *ReplicateVerifyResult) ReplicateVerifyVerdict {
	if r.AnyMissing() {
		return VerdictBroken
	}
	if r.AnyDrifted() {
		return VerdictDrifted
	}
	return VerdictConsistent
}

// errReplicaKeyMissing is returned by verifyKey when the key is
// DEFINITIVELY absent at dst (dst.Stat returned storage.ErrNotFound).
// Only this condition classifies the replica as broken. A transient
// dst error, or any src-side error, is returned as a plain error that
// does NOT match errors.Is(err, errReplicaKeyMissing) so the caller
// aborts the verify rather than falsely declaring the replica broken.
var errReplicaKeyMissing = errors.New("key missing at replica")

// verifyKey checks dst for key.  Returns (drift, err) where:
//   - err matches errors.Is(err, errReplicaKeyMissing) ↔ the key is
//     DEFINITIVELY MISSING at dst (dst.Stat → ErrNotFound)
//   - err non-nil but NOT errReplicaKeyMissing ↔ a transient/src
//     error the caller must propagate (do NOT classify as missing)
//   - drift=true ↔ the key is present but with mismatched
//     content (size compared by default; in --deep mode, body
//     SHA-256 compared as well)
//
// The expectedSize parameter is the size at src (from List
// metadata).  When zero, we Stat src to get the size; used for
// chunk keys where the List call upstream didn't carry size.
func verifyKey(ctx context.Context, src, dst storage.StoragePlugin, key string, expectedSize int64, opts ReplicateVerifyOptions) (bool, error) {
	dstInfo, err := dst.Stat(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// Definitively absent at the replica → missing.
			return false, fmt.Errorf("%w: dst.Stat: %v", errReplicaKeyMissing, err)
		}
		// Transient/backend error (throttling, network, permission):
		// we can't conclude the key is missing, so surface a plain
		// error the caller propagates instead of marking the replica
		// broken.
		return false, fmt.Errorf("dst.Stat: %w", err)
	}
	if expectedSize == 0 {
		srcInfo, err := src.Stat(ctx, key)
		if err != nil {
			// If src is missing the key, that's a primary-side
			// issue — we can't verify the replica without a
			// reference.  Surface as a not-actionable error so
			// the caller doesn't classify the replica as
			// missing.
			return false, fmt.Errorf("src.Stat: %w", err)
		}
		expectedSize = srcInfo.Size
	}
	if dstInfo.Size != expectedSize {
		return true, nil
	}
	if !opts.Deep {
		return false, nil
	}
	// Deep mode: fetch + compare bytes.  We don't compare in
	// constant memory (could stream + chunk-hash); for v1, fetch
	// both bodies + compare equality.  Acceptable trade-off:
	// deep mode is opt-in and runs against a small sample.
	srcBody, err := readAll(ctx, src, key)
	if err != nil {
		return false, fmt.Errorf("deep src.Get: %w", err)
	}
	dstBody, err := readAll(ctx, dst, key)
	if err != nil {
		return false, fmt.Errorf("deep dst.Get: %w", err)
	}
	if len(srcBody) != len(dstBody) {
		return true, nil
	}
	for i := range srcBody {
		if srcBody[i] != dstBody[i] {
			return true, nil
		}
	}
	return false, nil
}

// readAll Get + reads body fully.  Helper.
func readAll(ctx context.Context, sp storage.StoragePlugin, key string) ([]byte, error) {
	rc, err := sp.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// chunkKeysFromManifest fetches the manifest at key from src,
// parses it, and returns the chunk-storage keys for every
// referenced chunk (deduplicated).  Skips on parse error — the
// caller treats that as a primary-side concern, not a replica
// concern.
//
// We deliberately re-parse JSON here rather than depending on
// internal/backup to avoid an import cycle.  The fields we need
// (`files[].chunks[].hash`) are stable per the v1 contract.
func chunkKeysFromManifest(ctx context.Context, sp storage.StoragePlugin, key string) ([]string, error) {
	body, err := readAll(ctx, sp, key)
	if err != nil {
		return nil, err
	}
	hashes, err := parseChunkHashes(body)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(hashes))
	seen := map[string]struct{}{}
	for _, h := range hashes {
		k := chunkKeyFromHexHash(h)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// chunkKeyFromHexHash mirrors ChunkKey() for a string-shaped hash.
// Same 2/2/60 split that ChunkKey produces.
func chunkKeyFromHexHash(hex string) string {
	if len(hex) < 4 {
		return ""
	}
	return "chunks/sha256/" + hex[:2] + "/" + hex[2:4] + "/" + hex + ".chk"
}

// parseChunkHashes is a manual JSON walk over the manifest body
// that extracts every `files[].chunks[].hash` field.  We avoid
// pulling internal/backup here because it would create an import
// cycle (backup imports repo; this lives in repo).
func parseChunkHashes(body []byte) ([]string, error) {
	var manifest struct {
		Files []struct {
			Chunks []struct {
				Hash string `json:"hash"`
			} `json:"chunks"`
		} `json:"files"`
	}
	if err := jsonStrictUnmarshal(body, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	out := make([]string, 0, 64)
	for _, f := range manifest.Files {
		for _, c := range f.Chunks {
			if c.Hash != "" {
				out = append(out, c.Hash)
			}
		}
	}
	return out, nil
}

// jsonStrictUnmarshal forwards to encoding/json.Unmarshal.
// Wrapped so we can swap in a non-JSON parser later (CBOR /
// MessagePack manifest formats appear in the SPEC's deferred
// list) without touching every caller.
func jsonStrictUnmarshal(body []byte, into any) error {
	return jsonUnmarshalImpl(body, into)
}

// isManifestKey filters List() yields to the canonical
// manifest.json files.  Excludes tombstone markers, hold
// markers, attestations, and the _replicas/ tree.
func isManifestKey(key string) bool {
	if !strings.HasSuffix(key, "/manifest.json") {
		return false
	}
	if strings.HasPrefix(key, "manifests/_replicas/") {
		return false
	}
	if strings.HasPrefix(key, "manifests/_trash/") {
		return false
	}
	return true
}

// isWALSegmentManifestKey filters List() yields to WAL segment
// manifests: `wal/<dep>/<8hex-TLI>/<24hex>.json`. It matches on the
// 24-hex basename so it excludes the other `.json` artefacts that live
// under wal/ — gap-state records (`wal/<dep>/gaps/<tli>-<nanos>.json`)
// and in-flight staging temps (`*.json.tmp.<rand>`).
//
// (Previously this checked for a `.wal.json` suffix that the streamer
// never produces — segment manifests end in plain `.json` — so the WAL
// replica-verify silently considered ZERO segments and always reported
// "consistent," even against a replica missing all its WAL.)
func isWALSegmentManifestKey(key string) bool {
	if !strings.HasSuffix(key, ".json") || strings.Contains(key, ".json.tmp.") {
		return false
	}
	base := strings.TrimSuffix(key, ".json")
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if len(base) != 24 {
		return false
	}
	for i := 0; i < 24; i++ {
		c := base[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// isWALAuxKey matches the direct-byte WAL auxiliary files a DR replica
// needs for cross-failover PITR: timeline `.history`, `.backup`, and
// `.partial`.
func isWALAuxKey(key string) bool {
	return strings.HasSuffix(key, ".history") ||
		strings.HasSuffix(key, ".backup") ||
		strings.HasSuffix(key, ".partial")
}

// backupIDFromKey extracts the backup_id from a manifest key.
// "manifests/db1/backups/db1.full.X/manifest.json" → "db1.full.X".
// Returns empty string when the key shape doesn't match.
func backupIDFromKey(key string) string {
	const prefix = "manifests/"
	if !strings.HasPrefix(key, prefix) {
		return ""
	}
	rel := strings.TrimPrefix(key, prefix)
	parts := strings.Split(rel, "/")
	// rel = "<dep>/backups/<id>/manifest.json"
	if len(parts) < 4 || parts[1] != "backups" {
		return ""
	}
	return parts[2]
}

// recordReplicateVerifyFailure appends to res.Failures with the
// per-result cap.  Counter totals are NOT capped; this is purely
// for the JSON body's per-key detail.
func recordReplicateVerifyFailure(res *ReplicateVerifyResult, f ReplicateVerifyFailure) {
	if len(res.Failures) >= maxReplicateVerifyFailures {
		return
	}
	res.Failures = append(res.Failures, f)
}

func emitReplicateVerifyProgress(opts ReplicateVerifyOptions, stage, current, status string) {
	if opts.OnProgress == nil {
		return
	}
	opts.OnProgress(ReplicateVerifyProgress{
		Stage:   stage,
		Current: current,
		Status:  status,
	})
}
