// rotate.go — KEK rotation pass (re-wrap DEKs under a new key envelope, no chunk re-encryption).
package backup

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// RotateKEKSchema is the on-disk version tag for RotateKEKResult bodies.
const RotateKEKSchema = "pg_hardstorage.backup.rotate_kek.v1"

// maxRotateKEKFailures bounds the per-result Failures slice (same
// posture as Replicate / Heal / WALPrune). Counter totals stay
// unbounded; only per-key error detail is capped.
const maxRotateKEKFailures = 50

// RotateKEKOptions configures one KEK rotation pass.
//
// What rotation does: walks every visible (non-tombstoned) backup
// manifest in the repo, finds those wrapped with `OldKEKRef`,
// decrypts the wrapped DEK using `OldKEK`, re-wraps it under
// `NewKEK`, mutates the manifest's encryption block to record the
// new wrapping (`WrappedDEK` + `KEKRef` change; everything else
// stays bit-identical), re-signs the manifest, and atomically
// rewrites the body at the same key.
//
// What rotation does NOT do:
//
//   - Re-encrypt chunks. Chunks are sealed with the DEK itself
//     (AES-256-GCM, the DEK as the key); rewrapping the DEK under a
//     new KEK leaves its plaintext — and therefore every chunk's
//     bytes — unchanged. This is the elegance of envelope encryption
//     — rotating the KEK is O(manifest count), not O(chunk count).
//   - Touch unencrypted manifests. They have no DEK to rewrap.
//   - Touch manifests wrapped with a different KEK ref. This is
//     deliberate: in multi-tenant repos, rotating per-tenant KEKs
//     individually is the correct posture. The operator passes
//     `--old-kek-ref X` and only those manifests get rotated.
//   - Touch tombstoned manifests. They're scheduled for deletion;
//     re-encrypting their wrapped DEKs would be wasted work.
//
// Resumability: a rotation interrupted partway through can be
// re-run with the SAME `--old-kek-ref` + `--new-kek-ref`. Manifests
// that were already rotated (their `KEKRef == NewKEKRef`) are
// counted as `AlreadyRotated` and skipped. The operation is
// effectively idempotent.
//
// Per-manifest atomicity: we write the new body to a tmp key,
// delete the original, then rename tmp → original. There IS a
// small window where the original key is gone — operators are
// expected to run rotation during a maintenance window. The
// replica copy at `manifests/_replicas/<id>.manifest.json` is
// rewritten the same way; failure to rewrite the replica is
// reported but doesn't fail the rotation (consistent with the
// existing manifest-store posture: replica is best-effort
// redundancy).
type RotateKEKOptions struct {
	// OldKEKRef is the kek_ref currently on the manifests we're
	// rotating. Only manifests with this ref are touched.
	OldKEKRef string
	// OldKEK is the actual key bytes of OldKEKRef. Used to unwrap
	// each manifest's DEK.
	OldKEK [encryption.KeyLen]byte

	// NewKEKRef is the kek_ref to record on rotated manifests.
	NewKEKRef string
	// NewKEK is the new key bytes. Used to re-wrap each DEK.
	NewKEK [encryption.KeyLen]byte

	// Signer re-signs the rewritten manifest. Required — every
	// committed manifest carries a fresh attestation.
	Signer *Signer

	// Verifier validates the signature on each manifest BEFORE
	// rotation, so we don't trust an attacker-planted manifest.
	// Required.
	Verifier *Verifier

	// DryRun reports the plan without rewriting anything. Default
	// off; the CLI's safe-by-default `--apply` posture flips this.
	DryRun bool

	// OnProgress fires per manifest. Optional. Synchronous.
	OnProgress func(ev RotateKEKProgress)

	// RetainUntil + RetentionMode carry the repo's WORM policy so a rotated
	// manifest (and its re-synced replica) is re-locked. overwriteManifest
	// rewrites via tmp+rename with no retention, so without this a KEK
	// rotation on a compliance repo would leave the rewritten manifest
	// freely deletable. Zero RetainUntil → no lock (non-WORM repo).
	RetainUntil   time.Time
	RetentionMode storage.WORMMode
}

// RotateKEKProgress is the per-manifest callback shape.
type RotateKEKProgress struct {
	Deployment string
	BackupID   string
	Outcome    string // "rotated" | "would_rotate" | "already_rotated" | "skipped_unencrypted" | "skipped_different_kek" | "failed"
	Reason     string
}

// RotateKEKFailure records one manifest that couldn't be rotated.
type RotateKEKFailure struct {
	Deployment string `json:"deployment"`
	BackupID   string `json:"backup_id"`
	Err        string `json:"err"`
}

// RotateKEKResult is the structured outcome.
type RotateKEKResult struct {
	Schema     string    `json:"schema"`
	StartedAt  time.Time `json:"started_at"`
	StoppedAt  time.Time `json:"stopped_at"`
	DurationMS int64     `json:"duration_ms"`

	OldKEKRef string `json:"old_kek_ref"`
	NewKEKRef string `json:"new_kek_ref"`
	DryRun    bool   `json:"dry_run"`

	Considered          int `json:"considered"`
	Rotated             int `json:"rotated"`
	AlreadyRotated      int `json:"already_rotated"`
	SkippedUnencrypted  int `json:"skipped_unencrypted"`
	SkippedDifferentKEK int `json:"skipped_different_kek"`
	Failed              int `json:"failed"`

	// ReplicaFailures counts manifests where the primary rewrite
	// succeeded but the replica copy update failed. The primary is
	// authoritative; the replica is best-effort redundancy. Each
	// is also recorded in Failures for the per-key detail.
	ReplicaFailures int `json:"replica_failures,omitempty"`

	Failures []RotateKEKFailure `json:"failures,omitempty"`
}

// RotateKEK runs one rotation pass. See RotateKEKOptions for the
// semantics.
func RotateKEK(ctx context.Context, sp storage.StoragePlugin, opts RotateKEKOptions) (*RotateKEKResult, error) {
	if sp == nil {
		return nil, errors.New("backup rotate-kek: nil StoragePlugin")
	}
	if opts.OldKEKRef == "" {
		return nil, errors.New("backup rotate-kek: OldKEKRef is required")
	}
	if opts.NewKEKRef == "" {
		return nil, errors.New("backup rotate-kek: NewKEKRef is required")
	}
	if opts.OldKEKRef == opts.NewKEKRef {
		return nil, errors.New("backup rotate-kek: OldKEKRef == NewKEKRef (nothing to rotate)")
	}
	if opts.Signer == nil {
		return nil, errors.New("backup rotate-kek: Signer is required")
	}
	if opts.Verifier == nil {
		return nil, errors.New("backup rotate-kek: Verifier is required")
	}

	res := &RotateKEKResult{
		Schema:    RotateKEKSchema,
		StartedAt: time.Now().UTC(),
		OldKEKRef: opts.OldKEKRef,
		NewKEKRef: opts.NewKEKRef,
		DryRun:    opts.DryRun,
	}
	finish := func() {
		res.StoppedAt = time.Now().UTC()
		res.DurationMS = res.StoppedAt.Sub(res.StartedAt).Milliseconds()
	}

	store := NewManifestStore(sp)
	deployments, err := store.Deployments(ctx)
	if err != nil {
		finish()
		return res, fmt.Errorf("backup rotate-kek: enumerate deployments: %w", err)
	}
	sort.Strings(deployments)

	for _, deployment := range deployments {
		if err := ctx.Err(); err != nil {
			finish()
			return res, err
		}
		for m, lerr := range store.List(ctx, deployment, opts.Verifier) {
			if err := ctx.Err(); err != nil {
				finish()
				return res, err
			}
			res.Considered++
			if lerr != nil {
				// Unverified / corrupt manifest. We don't auto-skip —
				// surface as a failure so the operator's audit trail
				// shows it.
				recordRotateFailure(res, deployment, "<unknown>", lerr)
				res.Failed++
				emitRotateProgress(opts, deployment, "<unknown>", "failed", lerr.Error())
				continue
			}
			outcome, err := rotateOneManifest(ctx, sp, store, m, opts)
			if err != nil {
				recordRotateFailure(res, deployment, m.BackupID, err)
			}
			switch outcome {
			case rotateOutcomeRotated:
				res.Rotated++
				emitRotateProgress(opts, deployment, m.BackupID, "rotated", "")
			case rotateOutcomeWouldRotate:
				res.Rotated++
				emitRotateProgress(opts, deployment, m.BackupID, "would_rotate", "")
			case rotateOutcomeAlreadyRotated:
				res.AlreadyRotated++
				emitRotateProgress(opts, deployment, m.BackupID, "already_rotated", "")
			case rotateOutcomeSkippedUnencrypted:
				res.SkippedUnencrypted++
				emitRotateProgress(opts, deployment, m.BackupID, "skipped_unencrypted", "")
			case rotateOutcomeSkippedDifferentKEK:
				res.SkippedDifferentKEK++
				emitRotateProgress(opts, deployment, m.BackupID, "skipped_different_kek",
					fmt.Sprintf("manifest is wrapped with %q, want %q",
						m.Encryption.KEKRef, opts.OldKEKRef))
			case rotateOutcomeReplicaFailed:
				res.Rotated++
				res.ReplicaFailures++
				emitRotateProgress(opts, deployment, m.BackupID, "rotated",
					"primary OK, replica copy failed (best-effort)")
			default:
				res.Failed++
				emitRotateProgress(opts, deployment, m.BackupID, "failed",
					func() string {
						if err != nil {
							return err.Error()
						}
						return "unknown failure"
					}())
			}
		}
	}

	finish()
	return res, nil
}

type rotateOutcome int

const (
	rotateOutcomeFailed rotateOutcome = iota
	rotateOutcomeRotated
	rotateOutcomeWouldRotate
	rotateOutcomeAlreadyRotated
	rotateOutcomeSkippedUnencrypted
	rotateOutcomeSkippedDifferentKEK
	rotateOutcomeReplicaFailed
)

// rotateOneManifest classifies and (when not a dry-run) rewrites
// one manifest. Returns the outcome + any error encountered.
//
// Resumability: when the manifest's KEKRef already matches
// NewKEKRef, we treat this as already-rotated and skip cleanly.
// This makes a re-run after a partial failure idempotent.
func rotateOneManifest(ctx context.Context, sp storage.StoragePlugin, store *ManifestStore, m *Manifest, opts RotateKEKOptions) (rotateOutcome, error) {
	if m.Encryption == nil {
		return rotateOutcomeSkippedUnencrypted, nil
	}
	if m.Encryption.KEKRef == opts.NewKEKRef {
		// Primary already rotated. But the REPLICA may have been left
		// behind: a prior run rotated the primary and then failed (or
		// crashed) before re-writing the replica, so the replica still
		// holds the OLD wrapped DEK. Heal it now. Without this the
		// resume path skips the whole manifest and strands the replica —
		// which becomes undecryptable the moment the old KEK is retired,
		// and on a primary loss the backup is unrecoverable.
		return healStaleReplica(ctx, sp, m, opts)
	}
	if m.Encryption.KEKRef != opts.OldKEKRef {
		return rotateOutcomeSkippedDifferentKEK, nil
	}

	// Decrypt the wrapped DEK with the OLD KEK.
	wrappedOld, err := base64.StdEncoding.DecodeString(m.Encryption.WrappedDEK)
	if err != nil {
		return rotateOutcomeFailed, fmt.Errorf("decode wrapped_dek: %w", err)
	}
	dek, err := encryption.Unwrap(opts.OldKEK, wrappedOld)
	if err != nil {
		return rotateOutcomeFailed, fmt.Errorf("unwrap with OldKEK: %w (the supplied OldKEK does not match this manifest's wrapped_dek)", err)
	}

	// Re-wrap with the NEW KEK.
	wrappedNew, err := encryption.Wrap(opts.NewKEK, dek)
	if err != nil {
		return rotateOutcomeFailed, fmt.Errorf("wrap with NewKEK: %w", err)
	}

	if opts.DryRun {
		return rotateOutcomeWouldRotate, nil
	}

	// Mutate the manifest's encryption block. We reset Attestation
	// so the canonical-bytes computation in Sign produces the
	// canonical form (Sign sets Attestation back at the end).
	m.Encryption.WrappedDEK = base64.StdEncoding.EncodeToString(wrappedNew)
	m.Encryption.KEKRef = opts.NewKEKRef
	m.Attestation = nil

	if err := m.Sign(opts.Signer); err != nil {
		return rotateOutcomeFailed, fmt.Errorf("re-sign manifest: %w", err)
	}
	body, err := m.MarshalToBytes()
	if err != nil {
		return rotateOutcomeFailed, fmt.Errorf("marshal rewritten manifest: %w", err)
	}

	primaryKey := PrimaryPath(m.Deployment, m.BackupID)
	if err := overwriteManifest(ctx, sp, primaryKey, body, opts.RetainUntil, opts.RetentionMode); err != nil {
		return rotateOutcomeFailed, fmt.Errorf("rewrite primary %s: %w", primaryKey, err)
	}

	// Replica copy is best-effort.
	replicaKey := ReplicaPath(m.BackupID)
	// Only rewrite the replica if it exists. A missing replica is
	// not an error; replicate may have skipped it.
	if _, statErr := sp.Stat(ctx, replicaKey); statErr == nil {
		if err := overwriteManifest(ctx, sp, replicaKey, body, opts.RetainUntil, opts.RetentionMode); err != nil {
			return rotateOutcomeReplicaFailed, fmt.Errorf("rewrite replica %s: %w", replicaKey, err)
		}
	}

	return rotateOutcomeRotated, nil
}

// healStaleReplica brings the replica copy of an ALREADY-rotated primary
// up to date — the resume-path counterpart to the inline replica write in
// the fresh-rotation path. m is the verified primary (KEKRef ==
// NewKEKRef). Returns:
//
//   - AlreadyRotated  — no replica, or the replica is already in sync.
//   - WouldRotate     — dry-run; the replica is stale and would be healed.
//   - Rotated         — the stale replica was re-synced from the primary.
//   - ReplicaFailed   — the replica is stale but could not be re-synced.
//
// A stale replica re-synced here uses the primary's canonical signed
// bytes verbatim, so the two copies become byte-identical.
func healStaleReplica(ctx context.Context, sp storage.StoragePlugin, m *Manifest, opts RotateKEKOptions) (rotateOutcome, error) {
	replicaKey := ReplicaPath(m.BackupID)
	rc, err := sp.Get(ctx, replicaKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return rotateOutcomeAlreadyRotated, nil // no replica to heal
		}
		return rotateOutcomeReplicaFailed, fmt.Errorf("read replica %s: %w", replicaKey, err)
	}
	rb, rerr := io.ReadAll(io.LimitReader(rc, MaxManifestBytes+1))
	_ = rc.Close()
	if rerr != nil {
		return rotateOutcomeReplicaFailed, fmt.Errorf("read replica %s: %w", replicaKey, rerr)
	}
	if rm, perr := ParseAttestationless(rb); perr == nil && rm.Encryption != nil &&
		rm.Encryption.KEKRef == opts.NewKEKRef {
		return rotateOutcomeAlreadyRotated, nil // replica already in sync
	}
	// Replica is stale (old KEKRef), corrupt, or unparseable → re-sync it
	// from the already-rotated primary.
	if opts.DryRun {
		return rotateOutcomeWouldRotate, nil
	}
	body, err := m.MarshalToBytes()
	if err != nil {
		return rotateOutcomeReplicaFailed, fmt.Errorf("marshal primary for replica re-sync: %w", err)
	}
	if err := overwriteManifest(ctx, sp, replicaKey, body, opts.RetainUntil, opts.RetentionMode); err != nil {
		return rotateOutcomeReplicaFailed, fmt.Errorf("re-sync replica %s: %w", replicaKey, err)
	}
	return rotateOutcomeRotated, nil
}

// overwriteManifest writes body at key, replacing what's there.
// Sequence: write tmp → delete original → rename tmp → original.
// There IS a small window where the original is missing (between
// the delete and the rename). Operators run rotation during a
// maintenance window; the rotation primitive's contract surfaces
// this in the package docs.
func overwriteManifest(ctx context.Context, sp storage.StoragePlugin, key string, body []byte, retainUntil time.Time, mode storage.WORMMode) error {
	tmp := key + ".rotate." + randSuffix()
	if _, err := sp.Put(ctx, tmp, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	}); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := sp.Delete(ctx, key); err != nil {
		// Best-effort cleanup of tmp; the original-delete error is
		// what matters.
		_ = sp.Delete(ctx, tmp)
		return fmt.Errorf("delete original: %w", err)
	}
	if err := sp.RenameIfNotExists(ctx, tmp, key); err != nil {
		// Cleanup attempt, then surface.
		_ = sp.Delete(ctx, tmp)
		return fmt.Errorf("rename tmp -> original: %w", err)
	}
	// Re-apply the repo's WORM lock to the rewritten manifest so a rotation
	// on a compliance repo doesn't leave it deletable. Empty mode →
	// Compliance; non-WORM backends return ErrUnsupported, ignored.
	if !retainUntil.IsZero() {
		m := mode
		if m == "" {
			m = storage.WORMCompliance
		}
		if err := sp.SetRetention(ctx, key, retainUntil, m); err != nil &&
			!errors.Is(err, storage.ErrUnsupported) {
			return fmt.Errorf("lock rewritten manifest %s: %w", key, err)
		}
	}
	return nil
}

func emitRotateProgress(opts RotateKEKOptions, deployment, backupID, outcome, reason string) {
	if opts.OnProgress == nil {
		return
	}
	opts.OnProgress(RotateKEKProgress{
		Deployment: deployment,
		BackupID:   backupID,
		Outcome:    outcome,
		Reason:     reason,
	})
}

func recordRotateFailure(res *RotateKEKResult, deployment, backupID string, err error) {
	if len(res.Failures) >= maxRotateKEKFailures {
		return
	}
	res.Failures = append(res.Failures, RotateKEKFailure{
		Deployment: deployment,
		BackupID:   backupID,
		Err:        err.Error(),
	})
}
