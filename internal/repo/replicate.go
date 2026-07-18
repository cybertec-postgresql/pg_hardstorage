// replicate.go — Replicate: copy chunks/manifests/audit between repos with per-key failure budget.
package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdio "io"
	"sort"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// ReplicateSchema is the on-disk version tag for ReplicateResult bodies.
const ReplicateSchema = "pg_hardstorage.repo.replicate.v1"

// maxReplicateFailures bounds the per-result Failures slice. We
// don't want a million-key replicate that hits a transient backend
// failure on every key to balloon a JSON result body.
const maxReplicateFailures = 50

// ReplicateOptions tunes Replicate's behavior. Zero value is "copy
// every non-tombstoned backup manifest + its chunks; ignore WAL".
type ReplicateOptions struct {
	// DryRun computes the work but writes nothing to dst. Used by
	// operators to size a replicate run before committing to it
	// ("how much would this cost in egress?").
	DryRun bool

	// IncludeWAL, when true, also walks wal/<deployment>/<tli>/
	// segment manifests and copies them + their chunks. Off by
	// default — many operators only want backup-level redundancy in
	// the replica region.
	IncludeWAL bool

	// OnProgress, when non-nil, is invoked once per top-level
	// replication step (manifest, chunk, wal segment). Synchronous;
	// must return promptly. Pass nil for silent operation.
	OnProgress func(ev ReplicateProgress)

	// DstWORM is the DESTINATION repo's WORM retention policy (read from
	// its HSREPO). When set, every object Replicate writes to dst gets
	// the retention deadline + mode — so a WORM-configured DR replica is
	// actually immutable, matching what the CAS applies on the
	// backup-time write path. Without this the replica's objects are
	// freely deletable even though the destination repo was initialised
	// for compliance: an attacker who reaches the DR region (or an
	// insider) can wipe the "immutable" copy. Nil/empty → plain copy.
	DstWORM *WORMPolicy

	// AllowUnenforcedWORM permits replicating into a DstWORM-configured
	// repo whose backend can't actually enforce WORM (e.g. file://).
	// Default false: Replicate refuses rather than produce a replica the
	// operator believes is immutable but isn't — the same footgun guard
	// the CAS applies via WithRetentionAllowUnenforced.
	AllowUnenforcedWORM bool

	// Now is the clock for computing each object's RetainUntil
	// (deadline = Now + retention). Zero → time.Now() captured once at
	// Replicate start, so every object in a run shares one deadline.
	Now time.Time
}

// retentionPut returns the WORM PutOptions fields for a dst write under
// opts.DstWORM, or zero values when no policy is configured. opts.Now must
// already be resolved to a non-zero instant by Replicate.
func (o ReplicateOptions) retentionPut() (retainUntil time.Time, mode storage.WORMMode) {
	if o.DstWORM.IsZero() {
		return time.Time{}, storage.WORMNone
	}
	return o.DstWORM.RetainUntil(o.Now), storage.WORMMode(o.DstWORM.Mode)
}

// ReplicateProgress is the per-step callback shape.
type ReplicateProgress struct {
	Stage   string // "manifest" | "wal_manifest"
	Current string // the source key being processed
}

// ReplicateFailure records a single key that couldn't be copied. We
// surface up to maxReplicateFailures of these in the result so
// operators have something to grep when a run reports failures, but
// the bound keeps the JSON body O(1) regardless of failure count.
type ReplicateFailure struct {
	Key string `json:"key"`
	Err string `json:"err"`
}

// ReplicateResult is the structured outcome of a Replicate run. Every
// counter is "since this Replicate started" — Replicate is a single
// sweep, not a long-running daemon, so cumulative state lives only in
// the destination repo's chunk inventory.
type ReplicateResult struct {
	Schema     string    `json:"schema"`
	StartedAt  time.Time `json:"started_at"`
	StoppedAt  time.Time `json:"stopped_at"`
	DurationMS int64     `json:"duration_ms"`

	SourceURL  string `json:"source_url,omitempty"`
	DestURL    string `json:"dest_url,omitempty"`
	DryRun     bool   `json:"dry_run"`
	IncludeWAL bool   `json:"include_wal"`

	ManifestsConsidered int `json:"manifests_considered"`
	ManifestsCopied     int `json:"manifests_copied"`
	ManifestsSkipped    int `json:"manifests_skipped"`    // already at dst
	ManifestsTombstoned int `json:"manifests_tombstoned"` // skipped because src tombstoned
	ManifestsFailed     int `json:"manifests_failed"`

	ChunksConsidered int `json:"chunks_considered"`
	ChunksCopied     int `json:"chunks_copied"`
	ChunksSkipped    int `json:"chunks_skipped"` // already at dst
	ChunksMissing    int `json:"chunks_missing"` // referenced but absent at src
	ChunksFailed     int `json:"chunks_failed"`

	WALManifestsConsidered int `json:"wal_manifests_considered,omitempty"`
	WALManifestsCopied     int `json:"wal_manifests_copied,omitempty"`
	WALManifestsSkipped    int `json:"wal_manifests_skipped,omitempty"`
	WALManifestsFailed     int `json:"wal_manifests_failed,omitempty"`

	// WAL auxiliary files: timeline `.history`, `.backup`, and `.partial`
	// files. They are stored as direct bytes (not chunked) and are
	// REQUIRED for a DR repo to support PITR across a failover — without
	// the timeline history PG can't navigate the timeline switch.
	WALAuxConsidered int `json:"wal_aux_considered,omitempty"`
	WALAuxCopied     int `json:"wal_aux_copied,omitempty"`
	WALAuxSkipped    int `json:"wal_aux_skipped,omitempty"`
	WALAuxFailed     int `json:"wal_aux_failed,omitempty"`

	BytesCopied int64 `json:"bytes_copied"`

	Failures []ReplicateFailure `json:"failures,omitempty"`
}

// Replicate copies committed manifests and their referenced chunks
// from src to dst. Idempotent: chunks already at dst are skipped via
// Stat; manifests already at dst are skipped via IfNotExists Put.
//
// HSREPO at dst MUST already exist — Replicate refuses to operate
// against a destination that isn't a real repo. Use
// `pg_hardstorage repo init <dest-url>` to bootstrap the destination.
//
// Tombstoned backups are NOT replicated. The replica is for surviving
// loss of the primary repo, not for resurrecting soft-deleted
// backups. Once a backup is tombstoned at src, future Replicate calls
// skip its manifest. (Already-replicated tombstoned backups stay
// where they are — Replicate only adds; it doesn't prune.)
//
// Order of operations per manifest:
//  1. Read manifest body from src.
//  2. For each referenced chunk hash: Stat dst, copy if missing.
//  3. Put manifest body at dst with IfNotExists (skip if present).
//  4. Mirror manifests/_replicas/<id>.manifest.json if it exists at src.
//
// Order matters for the resilience invariant: chunks must be visible
// at dst before the manifest that references them. A crash mid-walk
// produces a destination repo with some manifests fully replicated and
// some not yet attempted — never a manifest at dst whose chunks are
// only at src.
//
// Encryption: chunks are copied byte-for-byte with their on-disk
// envelope intact. The replica region uses the same wrapped DEK as
// the source (it's stored in the manifest body), so an operator
// recovering from a primary loss needs the same KMS access. This is
// intentional — re-wrapping every chunk would change its hash and
// break the CAS contract.
func Replicate(ctx context.Context, src, dst storage.StoragePlugin, opts ReplicateOptions) (*ReplicateResult, error) {
	if src == nil || dst == nil {
		return nil, errors.New("repo replicate: src and dst plugins required")
	}
	res := &ReplicateResult{
		Schema:     ReplicateSchema,
		StartedAt:  time.Now().UTC(),
		DryRun:     opts.DryRun,
		IncludeWAL: opts.IncludeWAL,
	}
	finish := func() {
		res.StoppedAt = time.Now().UTC()
		res.DurationMS = res.StoppedAt.Sub(res.StartedAt).Milliseconds()
	}

	// Resolve the retention clock ONCE so every object in this run shares
	// a single deadline (now + retention), rather than drifting per-chunk.
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}

	// Verify dst is a real repo. The caller handles `repo init` —
	// Replicate only ever ADDS to an existing destination, never
	// initialises one. This makes the ordering explicit (operator
	// runs init once, replicate as often as they like).
	if _, err := dst.Stat(ctx, HSREPOFilename); err != nil {
		finish()
		if errors.Is(err, storage.ErrNotFound) {
			return res, ErrNotARepo
		}
		return res, fmt.Errorf("repo replicate: stat dst HSREPO: %w", err)
	}

	// Compliance guard: if the destination repo is WORM-configured but its
	// backend can't enforce retention, refuse rather than silently produce
	// a replica the operator believes is immutable but isn't. Mirrors the
	// CAS's retentionUnenforceable check on the backup-time write path.
	// Skipped on DryRun (no writes) and when the operator explicitly
	// accepts the gap.
	if !opts.DryRun && !opts.DstWORM.IsZero() && !opts.AllowUnenforcedWORM && !dst.Capabilities().WORM {
		finish()
		return res, fmt.Errorf("%w: destination repo is WORM-configured (%s) but backend %q does not enforce retention; the replica would be freely deletable. Use a WORM-capable destination or pass AllowUnenforcedWORM to accept the gap",
			ErrRetentionUnenforceable, opts.DstWORM.Mode, dst.Name())
	}
	var mutationLock *MutationLock
	if !opts.DryRun {
		var lockErr error
		mutationLock, lockErr = AcquireMutationLock(ctx, dst, "repository replication")
		if lockErr != nil {
			finish()
			return res, fmt.Errorf("repo replicate: mutation lock: %w", lockErr)
		}
		defer func() { _ = mutationLock.Release(context.Background()) }()
	}

	// Pass 1: collect tombstoned backup IDs at src. A separate pass
	// (rather than interleaving with the main walk) keeps the logic
	// O(N) without a second-list-during-iteration footgun.
	tombstoned := map[string]struct{}{}
	for info, err := range src.List(ctx, "manifests/") {
		if err != nil {
			finish()
			return res, fmt.Errorf("repo replicate: list src manifests: %w", err)
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

	// Pass 2: gather primary manifest keys.
	var primaryKeys []string
	for info, err := range src.List(ctx, "manifests/") {
		if err != nil {
			finish()
			return res, fmt.Errorf("repo replicate: list src manifests: %w", err)
		}
		if !strings.HasSuffix(info.Key, "/manifest.json") {
			continue
		}
		if strings.Contains(info.Key, ".tmp.") {
			continue
		}
		// _replicas/ holds redundancy copies that we'll mirror as a
		// follow-up to each primary commit, NOT walk independently.
		// Walking them as primaries would double the chunk-stat
		// traffic for zero benefit.
		if strings.HasPrefix(info.Key, "manifests/_replicas/") {
			continue
		}
		primaryKeys = append(primaryKeys, info.Key)
	}
	sort.Strings(primaryKeys)

	for _, key := range primaryKeys {
		if err := ctx.Err(); err != nil {
			finish()
			return res, err
		}
		res.ManifestsConsidered++

		// manifests/<dep>/backups/<id>/manifest.json
		parts := strings.Split(key, "/")
		var backupID string
		if len(parts) >= 5 {
			backupID = parts[3]
			if _, dead := tombstoned[backupID]; dead {
				res.ManifestsTombstoned++
				continue
			}
		}

		if opts.OnProgress != nil {
			opts.OnProgress(ReplicateProgress{Stage: "manifest", Current: key})
		}
		replicateManifest(ctx, src, dst, key, backupID, harvestBackup, opts, res)
	}

	// Pass 3: WAL — same flow, gated on opts.IncludeWAL.
	if opts.IncludeWAL {
		var walKeys, walAuxKeys []string
		for info, err := range src.List(ctx, "wal/") {
			if err != nil {
				finish()
				return res, fmt.Errorf("repo replicate: list src wal: %w", err)
			}
			key := info.Key
			if strings.Contains(key, ".tmp.") {
				continue // staging temp (manifest or .history)
			}
			switch {
			case strings.HasSuffix(key, ".json"):
				walKeys = append(walKeys, key)
			case strings.HasSuffix(key, ".history"),
				strings.HasSuffix(key, ".backup"),
				strings.HasSuffix(key, ".partial"):
				// Direct-byte aux files — timeline history, backup-label,
				// and partial segments. REQUIRED for PITR across a
				// failover at the DR site (the .history navigates the
				// timeline switch); copying only the .json segment
				// manifests would leave the replica unable to recover
				// across a timeline change.
				walAuxKeys = append(walAuxKeys, key)
			}
		}
		sort.Strings(walKeys)
		sort.Strings(walAuxKeys)
		for _, key := range walKeys {
			if err := ctx.Err(); err != nil {
				finish()
				return res, err
			}
			res.WALManifestsConsidered++
			if opts.OnProgress != nil {
				opts.OnProgress(ReplicateProgress{Stage: "wal_manifest", Current: key})
			}
			replicateWALManifest(ctx, src, dst, key, opts, res)
		}
		for _, key := range walAuxKeys {
			if err := ctx.Err(); err != nil {
				finish()
				return res, err
			}
			res.WALAuxConsidered++
			if opts.OnProgress != nil {
				opts.OnProgress(ReplicateProgress{Stage: "wal_aux", Current: key})
			}
			replicateWALAux(ctx, src, dst, key, opts, res)
		}
	}

	finish()
	return res, nil
}

// replicateManifest copies one primary backup manifest. Failures are
// recorded in res but do NOT abort the replicate run — operators
// running this from cron want the full report, not a fail-on-first.
func replicateManifest(ctx context.Context, src, dst storage.StoragePlugin, key, backupID string, kind harvestKind, opts ReplicateOptions, res *ReplicateResult) {
	body, err := readKey(ctx, src, key)
	if err != nil {
		recordReplicateFailure(res, key, err)
		res.ManifestsFailed++
		return
	}
	hashes, err := extractChunkHashes(body, kind)
	if err != nil {
		recordReplicateFailure(res, key, fmt.Errorf("decode manifest: %w", err))
		res.ManifestsFailed++
		return
	}

	// Chunks first — invariant: never a manifest at dst pointing at
	// chunks that aren't.
	for _, h := range hashes {
		if err := ctx.Err(); err != nil {
			return
		}
		copyChunk(ctx, src, dst, h, opts, res)
	}

	// Then the manifest body itself.
	if !copyKey(ctx, src, dst, key, body, opts, res, manifestKind, false /* not best-effort */) {
		return
	}

	// Mirror the _replicas/<id>.manifest.json copy if it exists at
	// src. The replica is best-effort redundancy; failure to copy
	// it does NOT bump ManifestsFailed (the primary already
	// committed at dst, which is the contract).
	if backupID != "" {
		replicaKey := "manifests/_replicas/" + backupID + ".manifest.json"
		if rb, rerr := readKey(ctx, src, replicaKey); rerr == nil {
			copyKey(ctx, src, dst, replicaKey, rb, opts, res, manifestKind, true /* best-effort */)
		}
	}
}

// replicateWALManifest copies one WAL segment manifest + its chunks.
// Same shape as replicateManifest but counts roll up to the WAL
// counters.
func replicateWALManifest(ctx context.Context, src, dst storage.StoragePlugin, key string, opts ReplicateOptions, res *ReplicateResult) {
	body, err := readKey(ctx, src, key)
	if err != nil {
		recordReplicateFailure(res, key, err)
		res.WALManifestsFailed++
		return
	}
	hashes, err := extractChunkHashes(body, harvestWAL)
	if err != nil {
		recordReplicateFailure(res, key, fmt.Errorf("decode wal manifest: %w", err))
		res.WALManifestsFailed++
		return
	}
	for _, h := range hashes {
		if err := ctx.Err(); err != nil {
			return
		}
		copyChunk(ctx, src, dst, h, opts, res)
	}
	copyKey(ctx, src, dst, key, body, opts, res, walKind, false)
}

// replicateWALAux copies one WAL auxiliary file (timeline `.history`,
// `.backup`, or `.partial`) verbatim. These are stored as direct bytes
// (no chunks, no manifest), so the copy is a single key-to-key transfer.
func replicateWALAux(ctx context.Context, src, dst storage.StoragePlugin, key string, opts ReplicateOptions, res *ReplicateResult) {
	body, err := readKey(ctx, src, key)
	if err != nil {
		recordReplicateFailure(res, key, err)
		res.WALAuxFailed++
		return
	}
	copyKey(ctx, src, dst, key, body, opts, res, walAuxKind, false)
}

// keyKind selects which counter set copyKey updates.
type keyKind int

const (
	manifestKind keyKind = iota
	walKind
	walAuxKind
)

// copyKey writes body to key at dst, idempotently. Returns true on
// success or skip (the caller can proceed); false on hard failure
// (the caller may want to short-circuit). bestEffort=true downgrades
// failures to log-only — failure to copy a replica is not a primary-
// replication failure.
func copyKey(ctx context.Context, src, dst storage.StoragePlugin, key string, body []byte, opts ReplicateOptions, res *ReplicateResult, kind keyKind, bestEffort bool) bool {
	if opts.DryRun {
		// Predict whether this would have been a copy or a skip.
		if _, serr := dst.Stat(ctx, key); errors.Is(serr, storage.ErrNotFound) {
			bumpKeyCopied(res, kind)
			res.BytesCopied += int64(len(body))
		} else if serr == nil {
			bumpKeySkipped(res, kind)
		}
		// We don't emit a "would-fail" prediction for stat errors
		// other than NotFound — a real run would still try the Put
		// and it might succeed.
		return true
	}
	putOpts := storage.PutOptions{IfNotExists: true, ContentLength: int64(len(body))}
	putOpts.RetainUntil, putOpts.RetentionMode = opts.retentionPut()
	_, err := dst.Put(ctx, key, bytes.NewReader(body), putOpts)
	switch {
	case err == nil:
		bumpKeyCopied(res, kind)
		res.BytesCopied += int64(len(body))
		return true
	case errors.Is(err, storage.ErrAlreadyExists):
		bumpKeySkipped(res, kind)
		return true
	default:
		if bestEffort {
			recordReplicateFailure(res, key, fmt.Errorf("put dst (best-effort): %w", err))
			return true
		}
		recordReplicateFailure(res, key, fmt.Errorf("put dst: %w", err))
		bumpKeyFailed(res, kind)
		return false
	}
}

func bumpKeyCopied(res *ReplicateResult, kind keyKind) {
	switch kind {
	case manifestKind:
		res.ManifestsCopied++
	case walKind:
		res.WALManifestsCopied++
	case walAuxKind:
		res.WALAuxCopied++
	}
}

func bumpKeySkipped(res *ReplicateResult, kind keyKind) {
	switch kind {
	case manifestKind:
		res.ManifestsSkipped++
	case walKind:
		res.WALManifestsSkipped++
	case walAuxKind:
		res.WALAuxSkipped++
	}
}

func bumpKeyFailed(res *ReplicateResult, kind keyKind) {
	switch kind {
	case manifestKind:
		res.ManifestsFailed++
	case walKind:
		res.WALManifestsFailed++
	case walAuxKind:
		res.WALAuxFailed++
	}
}

// copyChunk handles one chunk reference. Stats dst first (cheap),
// pulls from src + writes to dst on miss. Updates res.
func copyChunk(ctx context.Context, src, dst storage.StoragePlugin, h Hash, opts ReplicateOptions, res *ReplicateResult) {
	res.ChunksConsidered++
	chunkKey := ChunkKey(h)

	// Already at dst?
	switch _, err := dst.Stat(ctx, chunkKey); {
	case err == nil:
		res.ChunksSkipped++
		return
	case errors.Is(err, storage.ErrNotFound):
		// fall through to copy
	default:
		recordReplicateFailure(res, chunkKey, fmt.Errorf("stat dst: %w", err))
		res.ChunksFailed++
		return
	}

	// Pull from src.
	cb, err := readKey(ctx, src, chunkKey)
	if err != nil {
		// Source is missing the chunk. The src manifest is broken;
		// we report it but keep going so the operator sees the full
		// damage report.
		recordReplicateFailure(res, chunkKey, fmt.Errorf("read src: %w", err))
		res.ChunksMissing++
		return
	}
	if opts.DryRun {
		res.ChunksCopied++
		res.BytesCopied += int64(len(cb))
		return
	}
	putOpts := storage.PutOptions{IfNotExists: true, ContentLength: int64(len(cb))}
	putOpts.RetainUntil, putOpts.RetentionMode = opts.retentionPut()
	_, err = dst.Put(ctx, chunkKey, bytes.NewReader(cb), putOpts)
	switch {
	case err == nil:
		res.ChunksCopied++
		res.BytesCopied += int64(len(cb))
	case errors.Is(err, storage.ErrAlreadyExists):
		// Race: a concurrent replicate run won the put. Treat as a
		// skip — both copies have the same bytes (CAS contract).
		res.ChunksSkipped++
	default:
		recordReplicateFailure(res, chunkKey, fmt.Errorf("put dst: %w", err))
		res.ChunksFailed++
	}
}

// readKey reads an entire object's body. Used for manifests and chunks
// alike — bodies are small enough (manifests: KB, chunks: 4 KiB to
// 256 KiB by default) that an in-memory copy is the simplest correct
// implementation.
func readKey(ctx context.Context, sp storage.StoragePlugin, key string) ([]byte, error) {
	rc, err := sp.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return stdio.ReadAll(rc)
}

// extractChunkHashes JSON-decodes body and returns every chunk hash it
// references. Re-uses the partial-decode shapes already in gc.go so
// the two passes (GC and replicate) agree on what a "chunk reference"
// looks like.
func extractChunkHashes(body []byte, kind harvestKind) ([]Hash, error) {
	switch kind {
	case harvestBackup:
		var m backupManifestShape
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, err
		}
		var hashes []Hash
		for _, f := range m.Files {
			for _, c := range f.Chunks {
				if h, err := parseHexHash(c.Hash); err == nil {
					hashes = append(hashes, h)
				}
			}
		}
		return hashes, nil
	case harvestWAL:
		var m walManifestShape
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, err
		}
		var hashes []Hash
		for _, c := range m.Chunks {
			if h, err := parseHexHash(c.Hash); err == nil {
				hashes = append(hashes, h)
			}
		}
		return hashes, nil
	}
	return nil, fmt.Errorf("repo replicate: unknown harvest kind %v", kind)
}

// recordReplicateFailure appends to res.Failures with a length cap so
// a million-key run with transient failures doesn't balloon the JSON
// body. Counters (ManifestsFailed, ChunksFailed, ...) are unbounded —
// the cap only applies to the per-key error detail.
func recordReplicateFailure(res *ReplicateResult, key string, err error) {
	if len(res.Failures) >= maxReplicateFailures {
		return
	}
	res.Failures = append(res.Failures, ReplicateFailure{
		Key: key,
		Err: err.Error(),
	})
}
