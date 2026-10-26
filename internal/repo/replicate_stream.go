// replicate_stream.go — Replicate for manifests too large to hold in memory.
package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
)

// replicateStreamThreshold is the manifest size above which Replicate
// stops reading the manifest into memory. A 200 TB database's manifest
// is hundreds of GB; below the threshold the original in-memory path
// (byte-exact refresh comparison included) is used unchanged.
var replicateStreamThreshold int64 = 64 << 20

// replicateManifestStreamed is replicateManifest for a backup manifest of
// the given size: chunk references are streamed out of the source object
// one at a time, and the manifest body is copied and compared as a
// stream. Heap use is independent of the manifest size apart from the
// list of ADOPTED chunks (already present at dst), which the commit
// fence protocol still takes as a slice.
//
// The ordering invariant is unchanged: every referenced chunk is at dst
// before the manifest is Put, and any chunk failure withholds it.
func replicateManifestStreamed(ctx context.Context, src, dst storage.StoragePlugin, key, backupID string, size int64, opts ReplicateOptions, res *ReplicateResult) {
	rc, err := src.Get(ctx, key)
	if err != nil {
		recordReplicateFailure(res, key, err)
		res.ManifestsFailed++
		return
	}
	seen := spill.NewHashSet(spill.Options{})
	defer seen.Close()
	chunksOK := true
	adoptedSet := spill.NewHashSet(spill.Options{})
	defer adoptedSet.Close()
	adopted := spillSource{adoptedSet}
	err = streamBackupChunkRefs(rc, func(hex string) error {
		h, perr := parseHexHash(hex)
		if perr != nil {
			return unparseableReplicaRef(hex, perr)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !seen.Add(h) {
			return nil
		}
		switch copyChunk(ctx, src, dst, h, opts, res) {
		case chunkFailed:
			chunksOK = false
		case chunkAdopted:
			adoptedSet.Add(h)
		}
		return nil
	})
	_ = rc.Close()
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		recordReplicateFailure(res, key, fmt.Errorf("decode manifest: %w", err))
		res.ManifestsFailed++
		return
	}
	if err := adoptedSet.Err(); err != nil {
		recordReplicateFailure(res, key, fmt.Errorf("adopted-chunk set: %w", err))
		res.ManifestsFailed++
		return
	}
	if err := seen.Err(); err != nil {
		recordReplicateFailure(res, key, fmt.Errorf("chunk de-duplication set: %w", err))
		res.ManifestsFailed++
		return
	}
	if !chunksOK {
		recordReplicateFailure(res, key, errors.New("manifest withheld from replica: one or more referenced chunks failed to copy (see chunk failures above); re-run replicate after fixing the source"))
		res.ManifestsFailed++
		return
	}
	fence, ok := beginDstFence(ctx, dst, key, adopted, opts, res)
	if !ok {
		res.ManifestsFailed++
		return
	}
	if !reverifyAdopted(ctx, src, dst, key, adopted, opts, res, false) {
		recordReplicateFailure(res, key, errors.New("manifest withheld from replica: an adopted chunk vanished from the replica before the manifest committed (concurrent `repo gc --apply` on the replica?) and could not be re-copied; re-run replicate"))
		res.ManifestsFailed++
		return
	}
	if !copyKeyStreamed(ctx, src, dst, key, size, opts, res, false) {
		return
	}
	if !confirmDstFence(ctx, src, dst, key, fence, adopted, opts, res) {
		res.ManifestsFailed++
		return
	}
	mirrorManifestReplica(ctx, src, dst, backupID, opts, res)
}

// mirrorManifestReplica copies manifests/_replicas/<id>.manifest.json when
// the source has one. It is best-effort redundancy (the primary already
// committed at dst), but never silent: a replica that cannot be read or
// copied is recorded and counted in ManifestReplicasFailed. A sidecar of a
// huge backup is as large as the manifest itself, so anything above
// replicateStreamThreshold is copied as a stream rather than read whole.
func mirrorManifestReplica(ctx context.Context, src, dst storage.StoragePlugin, backupID string, opts ReplicateOptions, res *ReplicateResult) {
	if backupID == "" {
		return
	}
	replicaKey := "manifests/_replicas/" + backupID + ".manifest.json"
	st, serr := src.Stat(ctx, replicaKey)
	if errors.Is(serr, storage.ErrNotFound) {
		return // no sidecar: normal
	}
	if serr != nil {
		recordReplicateFailure(res, replicaKey, fmt.Errorf("stat manifest replica at src (not copied): %w", serr))
		res.ManifestReplicasFailed++
		return
	}
	if st.Size > replicateStreamThreshold {
		copyKeyStreamed(ctx, src, dst, replicaKey, st.Size, opts, res, true)
		return
	}
	rb, rerr := readKey(ctx, src, replicaKey)
	if rerr != nil {
		if errors.Is(rerr, storage.ErrNotFound) {
			return // raced with a delete
		}
		recordReplicateFailure(res, replicaKey, fmt.Errorf("read manifest replica at src (not copied): %w", rerr))
		res.ManifestReplicasFailed++
		return
	}
	copyKey(ctx, src, dst, replicaKey, rb, opts, res, manifestKind, true)
}

// copyKeyStreamed is copyKey + refreshKey for a manifest copied as a
// stream: idempotent (IfNotExists), and a differing existing object
// (kms rotate rewrote the source) is refreshed, not skipped.
//
// bestEffort (the _replicas/ sidecar) counts failures in
// ManifestReplicasFailed instead of ManifestsFailed and reports success so
// the primary's outcome is unaffected.
func copyKeyStreamed(ctx context.Context, src, dst storage.StoragePlugin, key string, size int64, opts ReplicateOptions, res *ReplicateResult, bestEffort bool) bool {
	fail := func(err error) bool {
		recordReplicateFailure(res, key, err)
		if bestEffort {
			res.ManifestReplicasFailed++
			return true
		}
		res.ManifestsFailed++
		return false
	}
	if opts.DryRun {
		if _, serr := dst.Stat(ctx, key); errors.Is(serr, storage.ErrNotFound) {
			res.ManifestsCopied++
			res.BytesCopied += size
		} else if serr == nil {
			if same, err := objectsEqual(ctx, src, dst, key); err == nil && !same {
				res.ManifestsRefreshed++
				res.BytesCopied += size
			} else {
				res.ManifestsSkipped++
			}
		}
		return true
	}
	put := func(ifNotExists bool) error {
		rc, err := src.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("read src: %w", err)
		}
		defer rc.Close()
		po := storage.PutOptions{IfNotExists: ifNotExists, ContentLength: size}
		po.RetainUntil, po.RetentionMode = opts.retentionPut()
		_, err = dst.Put(ctx, key, rc, po)
		return err
	}
	err := put(true)
	switch {
	case err == nil:
		res.ManifestsCopied++
		res.BytesCopied += size
		return true
	case errors.Is(err, storage.ErrAlreadyExists):
		same, cerr := objectsEqual(ctx, src, dst, key)
		if cerr != nil {
			return fail(fmt.Errorf("compare existing replica manifest: %w", cerr))
		}
		if same {
			res.ManifestsSkipped++
			return true
		}
		if err := put(false); err != nil {
			return fail(fmt.Errorf("replica holds a STALE copy (src was rewritten, e.g. by `kms rotate`) and the refresh was refused — a WORM lock on the replica's copy blocks it until the lock expires: %w", err))
		}
		res.ManifestsRefreshed++
		res.BytesCopied += size
		return true
	default:
		return fail(fmt.Errorf("put dst: %w", err))
	}
}

// objectsEqual compares the same key in two stores as streams.
func objectsEqual(ctx context.Context, a, b storage.StoragePlugin, key string) (bool, error) {
	ra, err := a.Get(ctx, key)
	if err != nil {
		return false, err
	}
	defer ra.Close()
	rb, err := b.Get(ctx, key)
	if err != nil {
		return false, err
	}
	defer rb.Close()
	const n = 1 << 20
	ba, bb := make([]byte, n), make([]byte, n)
	for {
		na, ea := io.ReadFull(ra, ba)
		nb, eb := io.ReadFull(rb, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false, nil
		}
		endA := errors.Is(ea, io.EOF) || errors.Is(ea, io.ErrUnexpectedEOF)
		endB := errors.Is(eb, io.EOF) || errors.Is(eb, io.ErrUnexpectedEOF)
		if ea != nil && !endA {
			return false, ea
		}
		if eb != nil && !endB {
			return false, eb
		}
		if endA || endB {
			return endA && endB, nil
		}
	}
}

// spillSource adapts a spill.HashSet to HashSource.
type spillSource struct{ s *spill.HashSet }

func (a spillSource) Len() int { return a.s.Len() }
func (a spillSource) ForEachHash(fn func(Hash) error) error {
	return a.s.ForEach(func(k spill.Key) error { return fn(Hash(k)) })
}
