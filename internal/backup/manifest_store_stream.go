package backup

import (
	"context"
	"errors"
	"fmt"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// StreamFiles is the constant-memory counterpart of Read for manifests
// too large to hold: it applies Read's tombstone gate, primary→replica
// fallback and replica identity check, verifies the signature under
// verifier (as Read does; Validate is a separate, explicit step), and
// then calls fn for every file
// entry in document order.
//
// Every entry is verified BEFORE fn sees the first one. The signature
// can only be checked after the last byte, so StreamFiles reads the
// object twice: a verifying pass with no callback, then a delivering
// pass whose SHA-256 must equal the first pass's. A caller that writes
// to disk from fn (restore) therefore never acts on unverified data,
// unless the object is swapped between the passes, which is reported as
// an error by the end of the second one.
//
// fn may be nil, which makes this a verifying summary read: the returned
// StreamInfo carries the header and the file, byte and chunk-reference
// counts. The returned Manifest has Files == nil.
func (ms *ManifestStore) StreamFiles(ctx context.Context, deployment, backupID string, verifier *Verifier, fn func(*FileEntry) error) (*StreamInfo, error) {
	if verifier == nil {
		return nil, errors.New("backup: StreamFiles requires a verifier (use StreamFilesAttestationless to read without one)")
	}
	return ms.streamFiles(ctx, deployment, backupID, StreamOptions{Verifier: verifier}, true, fn)
}

// StreamFilesAttestationless is StreamFiles without signature
// verification, for read-only inspection paths (ReadAttestationless'
// trust posture). Entries are delivered in a single pass.
func (ms *ManifestStore) StreamFilesAttestationless(ctx context.Context, deployment, backupID string, fn func(*FileEntry) error) (*StreamInfo, error) {
	return ms.streamFiles(ctx, deployment, backupID, StreamOptions{}, false, fn)
}

func (ms *ManifestStore) streamFiles(ctx context.Context, deployment, backupID string, opts StreamOptions, gate bool, fn func(*FileEntry) error) (*StreamInfo, error) {
	return ms.streamFilesT(ctx, deployment, backupID, opts, gate, fn, false)
}

// streamFilesT is streamFiles with the tombstone gate optional, for
// callers (show --include-deleted) that inspect soft-deleted manifests.
func (ms *ManifestStore) streamFilesT(ctx context.Context, deployment, backupID string, opts StreamOptions, gate bool, fn func(*FileEntry) error, allowTombstoned bool) (*StreamInfo, error) {
	if err := validateRef(deployment, backupID); err != nil {
		return nil, err
	}
	if !allowTombstoned {
		if dead, err := ms.IsTombstoned(ctx, deployment, backupID); err != nil {
			return nil, fmt.Errorf("backup: tombstone check %s: %w", backupID, err)
		} else if dead {
			return nil, ErrTombstoned
		}
	}
	primaryKey := PrimaryPath(deployment, backupID)
	info, key, primaryErr := ms.streamOne(ctx, primaryKey, opts, gate, fn)
	if primaryErr == nil {
		info.Key = key
		return info, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// A failure after delivery began (the caller's own error, or the
	// object changing mid-flight) is final: retrying on the replica
	// would hand the caller the same entries a second time.
	var de *deliveryError
	if errors.As(primaryErr, &de) {
		return nil, de.err
	}
	replicaKey := ReplicaPath(backupID)
	rinfo, rkey, replicaErr := ms.streamOne(ctx, replicaKey, opts, gate, fn)
	if replicaErr == nil {
		if rinfo.Manifest.Deployment == deployment && rinfo.Manifest.BackupID == backupID {
			rinfo.Key = rkey
			return rinfo, nil
		}
		replicaErr = fmt.Errorf("backup: replica identity %s/%s != requested %s/%s",
			rinfo.Manifest.Deployment, rinfo.Manifest.BackupID, deployment, backupID)
	}
	if errors.Is(primaryErr, storage.ErrNotFound) && errors.Is(replicaErr, storage.ErrNotFound) {
		return nil, fmt.Errorf("backup: manifest %s/%s: %w", deployment, backupID, storage.ErrNotFound)
	}
	return nil, fmt.Errorf("backup: read %s: primary failed (%w) and replica failed (%v)", backupID, primaryErr, replicaErr)
}

// streamOne reads one object: verify pass (when gate), then deliver pass.
func (ms *ManifestStore) streamOne(ctx context.Context, key string, opts StreamOptions, gate bool, fn func(*FileEntry) error) (*StreamInfo, string, error) {
	get := func(o StreamOptions, cb func(*FileEntry) error) (*StreamInfo, error) {
		rc, err := ms.sp.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return StreamManifest(rc, o, cb)
	}
	if !gate {
		delivered := false
		var cb func(*FileEntry) error
		if fn != nil {
			cb = func(fe *FileEntry) error { delivered = true; return fn(fe) }
		}
		info, err := get(opts, cb)
		if err != nil && delivered {
			err = &deliveryError{err}
		}
		return info, key, err
	}
	// Pass 1: verify + validate, no delivery.
	first, err := get(opts, nil)
	if err != nil {
		return nil, key, err
	}
	if fn == nil {
		return first, key, nil
	}
	// Pass 2: deliver. Verification already held for these exact bytes if
	// the hash matches, so only the hash needs re-checking.
	second, err := get(StreamOptions{}, fn)
	if err != nil {
		return nil, key, &deliveryError{err}
	}
	if second.RawSHA256 != first.RawSHA256 {
		return nil, key, &deliveryError{fmt.Errorf("backup: manifest %s changed between its verification pass and its delivery pass; entries already delivered must be discarded", key)}
	}
	first.Files, first.Bytes, first.ChunkRefs = second.Files, second.Bytes, second.ChunkRefs
	return first, key, nil
}

// deliveryError marks a failure that happened after entries reached the
// caller's callback.
type deliveryError struct{ err error }

func (e *deliveryError) Error() string { return e.err.Error() }
func (e *deliveryError) Unwrap() error { return e.err }
