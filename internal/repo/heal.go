// heal.go — Heal: refetch corrupt chunks from a replica + verify-post-write.
package repo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// HealSchema is the on-disk version tag for HealResult bodies.
const HealSchema = "pg_hardstorage.repo.heal.v1"

// maxHealFailures bounds the per-result Failures slice (same posture
// as Replicate). Counter totals (Failed) are unbounded; only per-key
// detail is capped.
const maxHealFailures = 50

// HealOptions tunes Heal's behavior. The zero value is "delete the
// local copy and refetch from replica; verify post-write".
type HealOptions struct {
	// DryRun reports the work but writes nothing to dst. Useful for
	// the operator's "would --heal actually fix this?" sanity check
	// before pulling the trigger.
	DryRun bool

	// SkipVerify omits the post-write SHA-256 round-trip check. Only
	// safe when the dst storage backend has its own end-to-end
	// checksum (S3 / Azure / GCS do). For fs:// this should stay
	// false — the cheap re-hash is what catches "the disk wrote
	// successfully but the bytes on the platter are wrong" cases.
	SkipVerify bool

	// OnProgress, when non-nil, is invoked once per healed hash.
	// Synchronous; must return promptly. Pass nil for silent.
	OnProgress func(ev HealProgress)

	// RetainUntil + RetentionMode carry the repo's WORM policy so a healed
	// chunk is re-locked on a compliance repo. The heal rewrites the chunk
	// with an IfNotExists Put, which carries no retention, so without this a
	// repair on a compliance repo would leave the healed chunk freely
	// deletable. Zero RetainUntil → no lock (non-WORM repo).
	RetainUntil   time.Time
	RetentionMode storage.WORMMode
}

// HealProgress is the per-hash callback shape.
type HealProgress struct {
	Hash    string
	Outcome string // "healed" | "already_ok" | "not_at_replica" | "failed"
}

// HealFailure records one hash that couldn't be healed.
type HealFailure struct {
	Hash string `json:"hash"`
	Err  string `json:"err"`
}

// HealResult is the structured outcome of a Heal run.
type HealResult struct {
	Schema     string    `json:"schema"`
	StartedAt  time.Time `json:"started_at"`
	StoppedAt  time.Time `json:"stopped_at"`
	DurationMS int64     `json:"duration_ms"`

	ReplicaURL string `json:"replica_url,omitempty"`
	DryRun     bool   `json:"dry_run"`

	Considered   int `json:"considered"`
	Healed       int `json:"healed"`
	AlreadyOK    int `json:"already_ok"`     // local copy verified clean (didn't need a heal)
	NotAtReplica int `json:"not_at_replica"` // replica is missing this chunk
	Failed       int `json:"failed"`

	BytesCopied int64 `json:"bytes_copied"`

	Failures []HealFailure `json:"failures,omitempty"`
}

// Heal repairs locally-corrupted chunks by re-fetching their bytes
// from a replica region. Use it after `repo scrub` (or
// `repair scrub`) reports mismatches: the replica's bytes are
// uncorrupted (chunks are byte-copied verbatim by `repo replicate`),
// so a successful heal restores the local copy to a state where the
// CAS's plaintext-SHA round-trip passes again.
//
// Per-hash flow:
//  1. Verify the replica has the chunk (Stat). Missing → NotAtReplica.
//  2. Pull bytes from the replica.
//  3. Verify the local copy isn't ALREADY clean (the operator may
//     have already healed it). If clean, AlreadyOK.
//  4. Delete the corrupt local chunk.
//  5. Put the replica's bytes at dst.
//  6. Optional post-write verify: read back through the local CAS
//     and confirm the plaintext SHA matches.
//
// The replica's chunk-envelope bytes are byte-identical to what was
// originally written (Replicate doesn't re-encrypt), so writing them
// at dst restores the chunk to a state the local CAS can decrypt and
// verify with no key changes.
//
// Heal does NOT take a list of failed hashes from a prior scrub run
// — that coupling would force callers to keep transient state across
// command invocations. Instead the caller passes the hashes
// explicitly. The CLI (repair scrub --heal) wires this by re-running
// scrub inline first and feeding the mismatches forward.
func Heal(ctx context.Context, dst, replica storage.StoragePlugin, hashes []Hash, opts HealOptions) (*HealResult, error) {
	if dst == nil || replica == nil {
		return nil, errors.New("repo heal: dst and replica plugins required")
	}
	res := &HealResult{
		Schema:    HealSchema,
		StartedAt: time.Now().UTC(),
		DryRun:    opts.DryRun,
	}
	finish := func() {
		res.StoppedAt = time.Now().UTC()
		res.DurationMS = res.StoppedAt.Sub(res.StartedAt).Milliseconds()
	}

	// Verify dst is a real repo before we touch it. Same posture as
	// Replicate — Heal only operates against bootstrapped repos.
	if _, err := dst.Stat(ctx, HSREPOFilename); err != nil {
		finish()
		if errors.Is(err, storage.ErrNotFound) {
			return res, ErrNotARepo
		}
		return res, fmt.Errorf("repo heal: stat dst HSREPO: %w", err)
	}
	if _, err := replica.Stat(ctx, HSREPOFilename); err != nil {
		finish()
		if errors.Is(err, storage.ErrNotFound) {
			return res, fmt.Errorf("repo heal: replica is not a repo: %w", ErrNotARepo)
		}
		return res, fmt.Errorf("repo heal: stat replica HSREPO: %w", err)
	}

	// Sort for deterministic processing order (helps tests, audit
	// logs, and human readers).
	sortedHashes := append([]Hash(nil), hashes...)
	sort.Slice(sortedHashes, func(i, j int) bool {
		return sortedHashes[i].String() < sortedHashes[j].String()
	})

	for _, h := range sortedHashes {
		if err := ctx.Err(); err != nil {
			finish()
			return res, err
		}
		res.Considered++
		healOne(ctx, dst, replica, h, opts, res)
	}

	finish()
	return res, nil
}

// healOne handles one hash. Failures are recorded in res but do NOT
// abort the run — operators want to see the full damage report.
func healOne(ctx context.Context, dst, replica storage.StoragePlugin, h Hash, opts HealOptions, res *HealResult) {
	chunkKey := ChunkKey(h)

	// 1. Replica must have the chunk.
	if _, err := replica.Stat(ctx, chunkKey); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			res.NotAtReplica++
			recordHealFailure(res, h, fmt.Errorf("not at replica"))
			emitProgress(opts, h, "not_at_replica")
			return
		}
		res.Failed++
		recordHealFailure(res, h, fmt.Errorf("stat replica: %w", err))
		emitProgress(opts, h, "failed")
		return
	}

	// 2. Pull bytes from replica.
	replicaBytes, err := readKey(ctx, replica, chunkKey)
	if err != nil {
		res.Failed++
		recordHealFailure(res, h, fmt.Errorf("read replica: %w", err))
		emitProgress(opts, h, "failed")
		return
	}

	// 3. Is the local copy already clean? Compare on-disk envelope
	//    bytes byte-for-byte. If they match, the operator may have
	//    already healed this chunk between scrub and heal — count it
	//    AlreadyOK, no write needed.
	//
	//    We compare envelope bytes rather than running the CAS plaintext
	//    verification because Heal is storage-layer: at this layer we
	//    don't have the encryption keys to round-trip. The envelope-byte
	//    comparison is a strict-superset of the plaintext check (if
	//    envelope bytes match, plaintext does too).
	if localBytes, lerr := readKey(ctx, dst, chunkKey); lerr == nil {
		if bytes.Equal(localBytes, replicaBytes) {
			res.AlreadyOK++
			emitProgress(opts, h, "already_ok")
			return
		}
	}

	if opts.DryRun {
		res.Healed++
		res.BytesCopied += int64(len(replicaBytes))
		emitProgress(opts, h, "healed")
		return
	}

	// 4. Delete the (presumably corrupt) local copy. Idempotent —
	//    Delete on a missing key is a no-op.
	if err := dst.Delete(ctx, chunkKey); err != nil {
		res.Failed++
		recordHealFailure(res, h, fmt.Errorf("delete local: %w", err))
		emitProgress(opts, h, "failed")
		return
	}

	// 5. Write the replica's bytes at the canonical key.
	_, err = dst.Put(ctx, chunkKey, bytes.NewReader(replicaBytes), storage.PutOptions{
		IfNotExists:   true, // race-safe: a concurrent heal can't double-write
		ContentLength: int64(len(replicaBytes)),
	})
	switch {
	case err == nil:
		// good
	case errors.Is(err, storage.ErrAlreadyExists):
		// Race: a concurrent heal already wrote it. The bytes are
		// the same (replica is the source of truth), so this is
		// effectively a no-op AlreadyOK. Bump Healed anyway since
		// we contributed to the result.
	default:
		res.Failed++
		recordHealFailure(res, h, fmt.Errorf("put local: %w", err))
		emitProgress(opts, h, "failed")
		return
	}

	// Re-apply the repo's WORM lock to the healed chunk. The Put above used
	// IfNotExists, which carries no retention, so on a compliance repo the
	// fresh chunk would otherwise be deletable. Empty mode → Compliance;
	// non-WORM backends return ErrUnsupported, which we ignore.
	if !opts.RetainUntil.IsZero() {
		mode := opts.RetentionMode
		if mode == "" {
			mode = storage.WORMCompliance
		}
		if err := dst.SetRetention(ctx, chunkKey, opts.RetainUntil, mode); err != nil &&
			!errors.Is(err, storage.ErrUnsupported) {
			res.Failed++
			recordHealFailure(res, h, fmt.Errorf("lock healed chunk: %w", err))
			emitProgress(opts, h, "failed")
			return
		}
	}

	// 6. Post-write verify: re-read the envelope and confirm its
	//    on-disk SHA-256 matches what we just wrote. Skipping this
	//    is opt-in for backends with their own end-to-end checksums.
	if !opts.SkipVerify {
		readback, rerr := readKey(ctx, dst, chunkKey)
		if rerr != nil {
			res.Failed++
			recordHealFailure(res, h, fmt.Errorf("post-write read: %w", rerr))
			emitProgress(opts, h, "failed")
			return
		}
		if got, want := sha256.Sum256(readback), sha256.Sum256(replicaBytes); got != want {
			res.Failed++
			recordHealFailure(res, h, fmt.Errorf("post-write hash mismatch"))
			emitProgress(opts, h, "failed")
			return
		}
	}

	res.Healed++
	res.BytesCopied += int64(len(replicaBytes))
	emitProgress(opts, h, "healed")
}

func emitProgress(opts HealOptions, h Hash, outcome string) {
	if opts.OnProgress == nil {
		return
	}
	opts.OnProgress(HealProgress{Hash: h.String(), Outcome: outcome})
}

func recordHealFailure(res *HealResult, h Hash, err error) {
	if len(res.Failures) >= maxHealFailures {
		return
	}
	res.Failures = append(res.Failures, HealFailure{
		Hash: h.String(),
		Err:  err.Error(),
	})
}

// readKey is shared with replicate.go (same package). Defined there.
