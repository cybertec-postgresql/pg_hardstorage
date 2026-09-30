package storage

// commit.go — publishing an object that must never be overwritten.
//
// Manifests, integrity roots, DSA and threshold records all have the
// same requirement: the object appears at its key complete or not at
// all, and a second writer is REJECTED rather than allowed to
// overwrite. Every one of them implemented it the same way — write
// `<key>.tmp.<rand>`, then RenameIfNotExists.
//
// That shape costs a DELETE on every commit. On S3 RenameIfNotExists is
// HeadObject + CopyObject(IfNoneMatch) + DeleteObject, so a repository
// used as an anti-ransomware copy of record accumulates a delete marker
// per WAL segment — and a bucket policy that treats deletes as an
// anomaly cannot host it (issue #45). It also requires a conditional
// COPY, which S3-compatible stores frequently do not implement even
// when they implement a conditional PUT.
//
// A backend that advertises ConditionalPut can do the whole thing in
// one call: a PUT publishes the object atomically, and If-None-Match
// rejects the second writer. No staging object, so nothing to delete.
//
// The staging path stays for backends that cannot: sftp against a
// server without hardlink@openssh.com is the live example, and there
// the tmp+rename is the only way to get exclusion at all.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// CommitExclusive publishes body at key, atomically, refusing to
// overwrite an existing object.
//
// Returns ErrAlreadyExists when key is already present — on both
// paths, so callers can treat it as one condition. Callers that need
// to distinguish an idempotent re-commit from a genuine conflict (a
// split-brain doppelgänger, say) read the stored object on that error.
//
// opts carries per-object settings — content length, WORM retention —
// exactly as a direct Put would. IfNotExists is set by this function;
// whatever the caller passes for it is ignored, since "may not
// overwrite" is the contract rather than an option.
func CommitExclusive(ctx context.Context, sp StoragePlugin, key string, body []byte, opts PutOptions) error {
	if sp == nil {
		return fmt.Errorf("storage: CommitExclusive requires a plugin")
	}
	opts.ContentLength = int64(len(body))

	if sp.Capabilities().ConditionalPut {
		opts.IfNotExists = true
		if _, err := sp.Put(ctx, key, bytes.NewReader(body), opts); err != nil {
			// ErrAlreadyExists passes through unwrapped: it is a
			// result, not a fault, and callers match on it.
			return err
		}
		return nil
	}

	// Fallback: stage, then claim the key by rename. Costs a delete,
	// and needs the backend's conditional COPY — which is why it is
	// the fallback rather than the default.
	//
	// Retention is NOT put on the staging object. A rename is a copy on
	// every object store, and a copy does not carry the source's lock
	// (S3 CopyObject without ObjectLockMode/RetainUntilDate writes an
	// unlocked object), so staging with the lock produced exactly the
	// wrong result: an UNLOCKED committed manifest and a LOCKED
	// temporary nobody could remove. The lock goes on the final key
	// after the rename instead.
	tmp := key + ".tmp." + commitSuffix()
	staged := opts
	staged.IfNotExists = false
	staged.RetainUntil = time.Time{}
	staged.RetentionMode = WORMNone
	if _, err := sp.Put(ctx, tmp, bytes.NewReader(body), staged); err != nil {
		return fmt.Errorf("storage: stage %s: %w", key, err)
	}
	if err := sp.RenameIfNotExists(ctx, tmp, key); err != nil {
		// Best-effort: the rename's failure is what the caller needs,
		// and a failed cleanup must not mask it. Detached context so a
		// cancelled ctx does not strand the temporary.
		cctx, cancel := CleanupContext(ctx)
		_ = sp.Delete(cctx, tmp)
		cancel()
		return err
	}
	// Only a WORM backend can honour the lock; elsewhere Put ignores
	// RetainUntil, and so does this path (the WORM gates upstream
	// refuse retention on such backends before we get here).
	if !opts.RetainUntil.IsZero() && sp.Capabilities().WORM {
		mode := opts.RetentionMode
		if mode == WORMNone {
			mode = WORMCompliance // regulatory-grade default, as Put
		}
		if err := sp.SetRetention(ctx, key, opts.RetainUntil, mode); err != nil {
			// An unlocked object must not be reported as committed. Take
			// it back down so a retry can publish it locked; if even that
			// fails, say so: the key now holds an unprotected object.
			cctx, cancel := CleanupContext(ctx)
			derr := sp.Delete(cctx, key)
			cancel()
			if derr != nil {
				return fmt.Errorf("storage: lock %s after commit: %w; rollback failed, an UNLOCKED object remains at the key: %w", key, err, derr)
			}
			return fmt.Errorf("storage: lock %s after commit: %w", key, err)
		}
	}
	return nil
}

// commitSuffix is a short random tag keeping concurrent stagers off
// each other's temporary object.
func commitSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not survivable for a staging name;
		// a collision would let two writers share one temporary.
		panic("storage: crypto/rand unavailable for staging suffix: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
