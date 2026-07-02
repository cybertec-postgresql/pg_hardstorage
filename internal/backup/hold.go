// hold.go — retention-immune Hold marker (legal/debug preservation of a backup).
package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Hold is a marker that a backup must not be deleted by retention,
// regardless of policy outcome. The classic operational case is a
// regulatory request ("preserve everything from this date range
// pending litigation") — but it's also useful for "I'm still
// debugging from this backup, please don't reap it."
//
// Layout choice: same shape as Tombstone, sibling to the primary
// manifest:
//
//	manifests/<deployment>/backups/<id>/manifest.json.hold
//
// Mirrors TombstonePath for two reasons:
//
//   - One List walk over manifests/ surfaces both holds and
//     tombstones if a future audit pass needs them (suffix-match).
//   - "release the hold" is a single Delete of the marker, no
//     cross-prefix bookkeeping (just like undelete-via-tombstone-
//     removal will be when ships it).
type Hold struct {
	Schema     string     `json:"schema"`
	BackupID   string     `json:"backup_id"`
	Deployment string     `json:"deployment"`
	HeldAt     time.Time  `json:"held_at"`
	Holder     string     `json:"holder,omitempty"`     // operator-set: e.g. "ops@acme.com"
	Reason     string     `json:"reason,omitempty"`     // e.g. "GDPR Art. 17 #4421"
	ExpiresAt  *time.Time `json:"expires_at,omitempty"` // nil = indefinite (legal-hold default)
}

// ActiveAt reports whether the hold actively protects the backup
// at the given moment. A hold with no ExpiresAt is always active
// (the legal-hold default — a regulatory hold sticks around until
// explicitly released). A hold with ExpiresAt is active only
// before that instant; on or after, the marker is still on disk
// but no longer protects the manifest from deletion.
//
// Used by SoftDelete + SoftDeleteCascade to decide whether a
// present marker actually applies. Used by `hold list` to tag
// expired holds for operator cleanup. The marker is NOT auto-
// removed on expiry — that's the operator's call (an audit
// trail of "this expired but nobody removed it" is sometimes
// itself a useful signal).
func (h *Hold) ActiveAt(now time.Time) bool {
	if h == nil {
		return false
	}
	if h.ExpiresAt == nil {
		return true
	}
	return now.Before(*h.ExpiresAt)
}

// HoldSchema is the on-disk version tag for Hold bodies.
const HoldSchema = "pg_hardstorage.hold.v1"

// HoldPath returns the marker key for a held backup. Sibling to
// PrimaryPath, like TombstonePath.
func HoldPath(deployment, backupID string) string {
	return PrimaryPath(deployment, backupID) + ".hold"
}

// holdSuffix is what HoldPath ends every hold key with. Defined
// once so List walkers don't reproduce the literal.
const holdSuffix = "/manifest.json.hold"

// PutHold writes a Hold marker with no expiry (indefinite —
// legal-hold default). Thin wrapper over PutHoldUntil that
// preserves the existing API for callers that don't care about
// auto-expiry.
//
// Returns ErrNotFound if the named backup doesn't exist — we won't
// pin a phantom.
func (ms *ManifestStore) PutHold(ctx context.Context, deployment, backupID, holder, reason string) error {
	return ms.PutHoldUntil(ctx, deployment, backupID, holder, reason, time.Time{})
}

// PutHoldUntil writes a Hold marker with an optional expiry. A
// zero expiresAt means "indefinite" (the legal-hold default); a
// non-zero expiresAt sets ExpiresAt on the marker so the hold
// auto-deactivates at that instant (the marker stays on disk for
// audit but no longer blocks deletion).
//
// Idempotent: a second PutHold/PutHoldUntil for the same
// (deployment, backupID) updates the holder/reason/expiry fields;
// HeldAt is preserved from the original so the audit-log duration
// doesn't reset on every edit.
//
// Use cases:
//   - `hold add` (no --until): regulatory legal hold, indefinite.
//   - `hold add --until 30d`: "I'm debugging from this backup,
//     hold for 2 weeks" — auto-expires so an operator forgetting
//     to release doesn't permanently pin the backup.
//   - `hold add --until "2027-01-01"`: bounded preservation
//     (litigation discovery period, audit window).
func (ms *ManifestStore) PutHoldUntil(ctx context.Context, deployment, backupID, holder, reason string, expiresAt time.Time) error {
	if deployment == "" || backupID == "" {
		return errors.New("backup: PutHold requires deployment and backupID")
	}
	// Validate the storage identifiers up front, mirroring the sibling
	// tombstone paths (SoftDelete etc. all validateRef before touching
	// storage). Without this, a hold could be written under a key derived
	// from an unsafe identifier that the tombstone paths would reject.
	if err := validateRef(deployment, backupID); err != nil {
		return err
	}
	// Refuse to hold a backup that doesn't exist.
	if _, err := ms.sp.Stat(ctx, PrimaryPath(deployment, backupID)); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("backup: hold %s/%s: %w", deployment, backupID, storage.ErrNotFound)
		}
		return fmt.Errorf("backup: hold-stat %s/%s: %w", deployment, backupID, err)
	}

	// Refuse to hold a tombstoned (soft-deleted) backup. The primary
	// manifest.json survives a SoftDelete (the tombstone is a sibling
	// marker), so the Stat above passes even on a deleted backup — but a
	// hold there is a lie: GC reaps a tombstoned backup's chunks after the
	// grace window without consulting holds. This is the symmetric guard
	// to SoftDelete's hold check; together they keep a backup from ever
	// being simultaneously held and tombstoned.
	if tombstoned, terr := ms.IsTombstoned(ctx, deployment, backupID); terr != nil {
		return fmt.Errorf("backup: hold tombstone-check %s/%s: %w", deployment, backupID, terr)
	} else if tombstoned {
		return &ManifestTombstonedError{Deployment: deployment, BackupID: backupID}
	}

	heldAt := time.Now().UTC()
	// If a hold is already in place, preserve its HeldAt — only
	// overwrite Holder/Reason/ExpiresAt. The "duration of the
	// hold" is the audit-log artefact; resetting it on every
	// edit would erase that history.
	hadExisting := false
	if existing, err := ms.GetHold(ctx, deployment, backupID); err == nil && existing != nil {
		heldAt = existing.HeldAt
		hadExisting = true
	}

	var expPtr *time.Time
	if !expiresAt.IsZero() {
		exp := expiresAt.UTC()
		expPtr = &exp
	}
	h := Hold{
		Schema:     HoldSchema,
		BackupID:   backupID,
		Deployment: deployment,
		HeldAt:     heldAt,
		Holder:     holder,
		Reason:     reason,
		ExpiresAt:  expPtr,
	}
	body, err := encodeJSON(&h)
	if err != nil {
		return fmt.Errorf("backup: encode hold: %w", err)
	}

	key := HoldPath(deployment, backupID)
	// the previous tmp+RenameIfNotExists, fall-back-to-
	// delete-then-rename pattern left a window between Delete(key) and
	// the second RenameIfNotExists where the hold marker was ABSENT.
	// A concurrent SoftDelete that observed the missing marker bypassed
	// legal-hold protection entirely — defeating the regulatory
	// guarantee.
	//
	// Fix: write atomically through the plugin's Put with IfNotExists
	// false.  The fs plugin's putOverwrite is an internal tmp+fsync+
	// rename+syncDir, so the marker key transitions from "old hold body"
	// directly to "new hold body" without an intervening absent state.
	// Cloud backends provide the same atomic-overwrite guarantee
	// natively.  No window remains for SoftDelete to slip through.
	if _, err := ms.sp.Put(ctx, key, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	}); err != nil {
		return fmt.Errorf("backup: install hold: %w", err)
	}

	// Write-then-verify, symmetric to SoftDelete's post-tombstone hold
	// re-check. A concurrent SoftDelete could have tombstoned this backup
	// in the window between the pre-check above and this write; both the
	// pre-check (here) and SoftDelete's hold pre-check would then have
	// observed clean state, and the hold would be left protecting nothing
	// (GC ignores holds on tombstoned backups). Re-read the tombstone now
	// that the marker is durable: if it's tombstoned, the SoftDelete won
	// the race — remove the hold we just wrote and refuse.
	//
	// Only roll back a hold WE created: a pre-existing hold means
	// SoftDelete could never have tombstoned this backup (its own hold
	// check refuses), so the tombstoned branch is unreachable for an edit
	// — but guarding on hadExisting guarantees we never delete an
	// operator's standing legal hold.
	if tombstoned, terr := ms.IsTombstoned(ctx, deployment, backupID); terr != nil {
		return fmt.Errorf("backup: hold tombstone re-check %s/%s: %w", deployment, backupID, terr)
	} else if tombstoned {
		if !hadExisting {
			if rmErr := ms.RemoveHold(ctx, deployment, backupID); rmErr != nil {
				return fmt.Errorf("backup: hold on %s raced a concurrent soft-delete and removing the just-written hold failed (the backup may now be held-and-tombstoned; run `backup hold remove %s`): %w",
					backupID, backupID, rmErr)
			}
		}
		return &ManifestTombstonedError{Deployment: deployment, BackupID: backupID}
	}
	return nil
}

// RemoveHold deletes the hold marker for deployment/backupID. Idempotent:
// if the marker is already absent, returns nil.
func (ms *ManifestStore) RemoveHold(ctx context.Context, deployment, backupID string) error {
	if deployment == "" || backupID == "" {
		return errors.New("backup: RemoveHold requires deployment and backupID")
	}
	if err := ms.sp.Delete(ctx, HoldPath(deployment, backupID)); err != nil {
		// fs's Delete swallows ErrNotExist; other backends may not.
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("backup: delete hold: %w", err)
	}
	return nil
}

// IsHeld reports whether deployment/backupID has a hold marker
// PRESENT on disk — regardless of whether the hold is active.
// Cheap (single Stat). Used by `hold list` and forensics where
// the marker's existence is what matters.
//
// For "is this manifest actively protected from deletion right
// now?", use IsActivelyHeld instead — that respects ExpiresAt.
//
// (false, nil) on absent; (true, nil) on present; (false, err) on
// real backend errors.
func (ms *ManifestStore) IsHeld(ctx context.Context, deployment, backupID string) (bool, error) {
	_, err := ms.sp.Stat(ctx, HoldPath(deployment, backupID))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	return false, err
}

// IsActivelyHeld reports whether the manifest is actually
// protected from deletion right now — i.e. has a marker AND the
// marker's ExpiresAt (if set) is in the future. The retention
// pre-filter and SoftDelete's defense-in-depth check both want
// these semantics.
//
// One body read per call. (false, nil) on absent or expired;
// (true, nil) on actively held; (false, err) on real backend
// errors. A body that fails to decode is treated as "I don't
// know" — surfaced as an error so callers don't accidentally
// proceed past a corrupted hold.
func (ms *ManifestStore) IsActivelyHeld(ctx context.Context, deployment, backupID string) (bool, error) {
	h, err := ms.GetHold(ctx, deployment, backupID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return h.ActiveAt(time.Now().UTC()), nil
}

// GetHold reads the hold marker. Returns (nil, ErrNotFound)-wrapped
// when absent, or (*Hold, nil) on success. Used by PutHold to
// preserve HeldAt across edits, and by `hold list` to surface the
// holder/reason.
func (ms *ManifestStore) GetHold(ctx context.Context, deployment, backupID string) (*Hold, error) {
	rc, err := ms.sp.Get(ctx, HoldPath(deployment, backupID))
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	// Cap the read like the manifest/tombstone/lease paths do: an
	// unbounded io.ReadAll over a corrupt or maliciously oversized hold
	// object would OOM every hold path (IsActivelyHeld, ListHolds,
	// PutHoldUntil's HeldAt-preserve read, the retention pre-filter).
	// Same MaxManifestBytes cap as readAllLimited's manifest callers.
	body, err := readAllLimited(rc, MaxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("backup: read hold: %w", err)
	}
	var h Hold
	if err := json.Unmarshal(body, &h); err != nil {
		return nil, fmt.Errorf("backup: decode hold %s/%s: %w", deployment, backupID, err)
	}
	if h.Schema != HoldSchema {
		return nil, fmt.Errorf("backup: hold %s/%s has unsupported schema %q",
			deployment, backupID, h.Schema)
	}
	return &h, nil
}

// ExpiredHoldRemoved is the per-marker record returned by
// PurgeExpiredHolds. The metadata mirrors what ListHolds
// returned plus the original Hold body so callers (audit
// emit, CLI result body, dry-run preview) have a complete
// picture without re-reading the marker after deletion.
type ExpiredHoldRemoved struct {
	Deployment string
	BackupID   string
	Holder     string
	Reason     string
	HeldAt     time.Time
	ExpiredAt  time.Time // == *Hold.ExpiresAt at removal time
}

// PurgeExpiredHolds removes every hold marker whose ExpiresAt
// is in the past for the named deployment (or fleet-wide when
// deployment is ""). Indefinite holds (ExpiresAt == nil) are
// NEVER removed — they're the legal-hold default and the only
// way out is an explicit `hold remove`.
//
// Returns the metadata of every removed marker so callers can:
//   - emit one audit event per removal (per-marker forensic
//     record, not a single bulk event with a slice)
//   - render a structured CLI body listing what was reaped
//
// Atomicity: this function is NOT atomic across the set —
// each marker is deleted independently, and a mid-walk failure
// returns the partial list of successes plus the error. The
// operation is idempotent, so re-running on partial state
// completes the rest. Same posture as SoftDeleteCascade.
//
// `dryRun=true` walks + identifies expired markers without
// deleting any, so callers can preview the operation.
func (ms *ManifestStore) PurgeExpiredHolds(ctx context.Context, deployment string, dryRun bool) ([]ExpiredHoldRemoved, error) {
	holds, err := ms.ListHolds(ctx, deployment)
	if err != nil {
		return nil, fmt.Errorf("backup: PurgeExpiredHolds: list: %w", err)
	}
	now := time.Now().UTC()
	out := make([]ExpiredHoldRemoved, 0)
	for _, h := range holds {
		// Indefinite holds are never expired. Active bounded
		// holds (future ExpiresAt) are still protecting.
		if h.ExpiresAt == nil || h.ActiveAt(now) {
			continue
		}
		entry := ExpiredHoldRemoved{
			Deployment: h.Deployment,
			BackupID:   h.BackupID,
			Holder:     h.Holder,
			Reason:     h.Reason,
			HeldAt:     h.HeldAt,
			ExpiredAt:  *h.ExpiresAt,
		}
		if dryRun {
			out = append(out, entry)
			continue
		}
		// Re-read the hold immediately before deleting and confirm it is
		// STILL expired. The ListHolds snapshot can be seconds stale, and
		// a concurrent `hold add --until <future>` (PutHoldUntil) could
		// have RENEWED this hold to active in the meantime — removing it
		// from the stale snapshot would delete an active legal hold
		// (race-condition audit #2). Compare-and-delete on current state
		// narrows the window to the two adjacent storage ops below.
		cur, gerr := ms.GetHold(ctx, h.Deployment, h.BackupID)
		if gerr != nil {
			if errors.Is(gerr, storage.ErrNotFound) {
				continue // already removed concurrently — nothing to do
			}
			return out, fmt.Errorf("backup: purge expired hold %s/%s: re-read: %w", h.Deployment, h.BackupID, gerr)
		}
		if cur == nil || cur.ExpiresAt == nil || cur.ActiveAt(now) {
			continue // renewed to active (or made indefinite) since the snapshot — keep it
		}
		if err := ms.RemoveHold(ctx, h.Deployment, h.BackupID); err != nil {
			return out, fmt.Errorf("backup: purge expired hold %s/%s: %w", h.Deployment, h.BackupID, err)
		}
		out = append(out, entry)
	}
	return out, nil
}

// ListHolds walks every deployment's manifest tree and yields the
// parsed Hold for each marker. Optionally scoped to a single
// deployment (pass "" for fleet-wide).
func (ms *ManifestStore) ListHolds(ctx context.Context, deployment string) ([]*Hold, error) {
	prefix := "manifests/"
	if deployment != "" {
		prefix = "manifests/" + deployment + "/backups/"
	}
	var out []*Hold
	for info, err := range ms.sp.List(ctx, prefix) {
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(info.Key, holdSuffix) {
			continue
		}
		// Recover deployment + backupID from the key. Layout:
		//   manifests/<dep>/backups/<id>/manifest.json.hold
		parts := strings.Split(info.Key, "/")
		if len(parts) < 5 {
			continue
		}
		dep := parts[1]
		backupID := parts[3]
		h, err := ms.GetHold(ctx, dep, backupID)
		if err != nil {
			// A torn or legacy hold marker shouldn't fail the whole
			// listing; surface as a "couldn't decode" entry so the
			// operator sees something is wrong without losing the
			// rest of the list.
			out = append(out, &Hold{
				Schema:     HoldSchema,
				Deployment: dep,
				BackupID:   backupID,
				Reason:     "[unreadable: " + err.Error() + "]",
			})
			continue
		}
		out = append(out, h)
	}
	return out, nil
}
