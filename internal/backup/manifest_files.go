package backup

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"iter"
	"sort"
	"strings"
)

// ErrStreamedManifest is returned when a manifest read with ReadStreaming
// is asked to do something that needs its file list in memory
// (canonicalise, sign, marshal).
var ErrStreamedManifest = errors.New("backup: manifest was read with ReadStreaming and does not hold its file list; it is read-only")

// fileStream is how a streamed Manifest gets its entries back.
type fileStream struct {
	ms  *ManifestStore
	key string

	sha                     [32]byte
	files, bytes, chunkRefs int64
}

// Streaming reports whether m was read with ReadStreaming.
func (m *Manifest) Streaming() bool { return m.stream != nil }

// NumFiles is the number of file entries, whether held or streamed.
func (m *Manifest) NumFiles() int {
	if m.stream != nil {
		return int(m.stream.files)
	}
	return len(m.Files)
}

// TotalFileBytes is the summed Size of every file entry.
func (m *Manifest) TotalFileBytes() int64 {
	if m.stream != nil {
		return m.stream.bytes
	}
	var n int64
	for i := range m.Files {
		n += m.Files[i].Size
	}
	return n
}

// NumChunkRefs is the total number of chunk references (with repeats).
func (m *Manifest) NumChunkRefs() int64 {
	if m.stream != nil {
		return m.stream.chunkRefs
	}
	var n int64
	for i := range m.Files {
		n += int64(len(m.Files[i].Chunks))
	}
	return n
}

// CanonicalSHA256 is the SHA-256 of the manifest's canonical signing
// bytes (sha256 of Canonicalize()) whether m is held or streamed. A
// streamed manifest is hashed in one pass over the stored object with
// the canonical form spooled to disk, so memory stays independent of
// the file count; the pass fails if the object no longer hashes to what
// ReadStreaming verified.
func (m *Manifest) CanonicalSHA256(ctx context.Context) ([32]byte, error) {
	if m.stream == nil {
		b, err := m.Canonicalize()
		if err != nil {
			return [32]byte{}, err
		}
		return sha256.Sum256(b), nil
	}
	fs := m.stream
	rc, err := fs.ms.sp.Get(ctx, fs.key)
	if err != nil {
		return [32]byte{}, err
	}
	defer rc.Close()
	info, err := StreamManifest(rc, StreamOptions{CanonicalHash: true}, nil)
	if err != nil {
		return [32]byte{}, err
	}
	if info.RawSHA256 != fs.sha {
		return [32]byte{}, fmt.Errorf("backup: manifest %s changed after it was verified", fs.key)
	}
	return info.CanonicalSHA256, nil
}

// EachFile calls fn for every file entry in document order, from memory
// or from the store. For a streamed manifest the entry is only valid
// during the call, and each EachFile call is one pass over the stored
// object; the pass fails at its end if the object no longer hashes to
// what ReadStreaming verified. fn's own error stops the walk and is
// returned unchanged.
func (m *Manifest) EachFile(ctx context.Context, fn func(*FileEntry) error) error {
	if m.stream == nil {
		for i := range m.Files {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := fn(&m.Files[i]); err != nil {
				return err
			}
		}
		return nil
	}
	fs := m.stream
	rc, err := fs.ms.sp.Get(ctx, fs.key)
	if err != nil {
		return err
	}
	defer rc.Close()
	var n int64
	info, err := StreamManifest(rc, StreamOptions{}, func(fe *FileEntry) error {
		if n&0x3ff == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		n++
		return fn(fe)
	})
	if err != nil {
		return err
	}
	if info.RawSHA256 != fs.sha {
		return fmt.Errorf("backup: manifest %s changed after it was verified; entries already delivered must be discarded", fs.key)
	}
	return nil
}

// ReadStreaming is Read for manifests that must not be held in memory.
// It verifies the signature and every manifest invariant (one pass over
// the stored object), then returns the header with Files == nil;
// EachFile walks the entries on demand. Heap use is independent of the
// number of files. Tombstone gate, primary→replica fallback and replica
// identity check are Read's.
func (ms *ManifestStore) ReadStreaming(ctx context.Context, deployment, backupID string, verifier *Verifier) (*Manifest, error) {
	info, err := ms.StreamFiles(ctx, deployment, backupID, verifier, nil)
	if err != nil {
		return nil, err
	}
	m := info.Manifest
	m.stream = &fileStream{
		ms: ms, key: info.Key, sha: info.RawSHA256,
		files: info.Files, bytes: info.Bytes, chunkRefs: info.ChunkRefs,
	}
	return m, nil
}

// validate runs the full streaming Validate over the stored object and
// confirms it is still the object ReadStreaming verified.
func (fs *fileStream) validate() error {
	rc, err := fs.ms.sp.Get(context.Background(), fs.key)
	if err != nil {
		return err
	}
	defer rc.Close()
	info, err := StreamManifest(rc, StreamOptions{Validate: true}, nil)
	if err != nil {
		return err
	}
	if info.RawSHA256 != fs.sha {
		return fmt.Errorf("backup: manifest %s changed after it was verified", fs.key)
	}
	return nil
}

func (ms *ManifestStore) listStreamingEntries(ctx context.Context, deployment string, opts StreamOptions, ok bool, includeTombstoned bool, auto bool) iter.Seq2[ManifestEntry, error] {
	return func(yield func(ManifestEntry, error) bool) {
		if !ok {
			yield(ManifestEntry{}, errors.New("backup: streaming list requires a verifier"))
			return
		}
		if err := validateStorageID("deployment", deployment); err != nil {
			yield(ManifestEntry{}, err)
			return
		}
		prefix := "manifests/" + deployment + "/backups/"
		const manifestSuffix = "/manifest.json"
		const tombstoneSuffix = "/manifest.json.tombstone"
		var keys []string
		sizes := map[string]int64{}
		tombstoned := map[string]struct{}{}
		for info, err := range ms.sp.List(ctx, prefix) {
			if err != nil {
				yield(ManifestEntry{}, err)
				return
			}
			switch {
			case strings.HasSuffix(info.Key, tombstoneSuffix):
				rel := strings.TrimPrefix(info.Key, prefix)
				if slash := strings.IndexByte(rel, '/'); slash > 0 {
					tombstoned[rel[:slash]] = struct{}{}
				}
			case strings.HasSuffix(info.Key, manifestSuffix):
				keys = append(keys, info.Key)
				sizes[info.Key] = info.Size
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			rel := strings.TrimPrefix(key, prefix)
			dead := false
			if slash := strings.IndexByte(rel, '/'); slash > 0 {
				_, dead = tombstoned[rel[:slash]]
			}
			if dead && !includeTombstoned {
				continue
			}
			var (
				m   *Manifest
				err error
			)
			if auto && sizes[key] <= AutoStreamThreshold {
				m, err = ms.readHeld(ctx, key, opts)
			} else {
				m, err = ms.streamHeader(ctx, key, opts)
			}
			if err != nil {
				if !yield(ManifestEntry{}, err) {
					return
				}
				continue
			}
			if !yield(ManifestEntry{Manifest: m, Tombstoned: dead}, nil) {
				return
			}
		}
	}
}

// streamHeader reads one object into a streamed Manifest.
func (ms *ManifestStore) streamHeader(ctx context.Context, key string, opts StreamOptions) (*Manifest, error) {
	rc, err := ms.sp.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	info, err := StreamManifest(rc, opts, nil)
	if err != nil {
		return nil, err
	}
	m := info.Manifest
	m.stream = &fileStream{
		ms: ms, key: key, sha: info.RawSHA256,
		files: info.Files, bytes: info.Bytes, chunkRefs: info.ChunkRefs,
	}
	return m, nil
}

// ReadStreamingAttestationless is ReadStreaming without signature
// verification, for read-only inspection (ReadAttestationless' posture).
func (ms *ManifestStore) ReadStreamingAttestationless(ctx context.Context, deployment, backupID string) (*Manifest, error) {
	info, err := ms.StreamFilesAttestationless(ctx, deployment, backupID, nil)
	if err != nil {
		return nil, err
	}
	m := info.Manifest
	m.stream = &fileStream{
		ms: ms, key: info.Key, sha: info.RawSHA256,
		files: info.Files, bytes: info.Bytes, chunkRefs: info.ChunkRefs,
	}
	return m, nil
}

// AutoStreamThreshold is the stored-manifest size above which the *Auto
// readers hand back a streamed manifest (Files == nil, entries via
// EachFile) instead of a held one. Below it they behave exactly like
// Read/List: one read, Files populated. 64 MiB is ~550K chunk
// references, ~35 GB of database at the default chunk size — far under
// the 1 GiB cap those readers enforce, so nothing that fits today
// changes behaviour.
var AutoStreamThreshold int64 = 64 << 20

// ReadAuto is Read for callers that handle both shapes: it returns a held
// manifest when the primary object is at most AutoStreamThreshold and a
// streamed one otherwise. Callers must reach the files only through
// EachFile/NumFiles/TotalFileBytes/NumChunkRefs (which work on both).
func (ms *ManifestStore) ReadAuto(ctx context.Context, deployment, backupID string, verifier *Verifier) (*Manifest, error) {
	if ms.bigPrimary(ctx, deployment, backupID) {
		return ms.ReadStreaming(ctx, deployment, backupID, verifier)
	}
	return ms.Read(ctx, deployment, backupID, verifier)
}

// ReadAttestationlessAuto is ReadAuto for ReadAttestationless'
// trust posture.
func (ms *ManifestStore) ReadAttestationlessAuto(ctx context.Context, deployment, backupID string) (*Manifest, error) {
	if ms.bigPrimary(ctx, deployment, backupID) {
		return ms.ReadStreamingAttestationless(ctx, deployment, backupID)
	}
	return ms.ReadAttestationless(ctx, deployment, backupID)
}

// ReadIncludingTombstonedAuto is ReadAuto for ReadIncludingTombstoned.
func (ms *ManifestStore) ReadIncludingTombstonedAuto(ctx context.Context, deployment, backupID string, verifier *Verifier) (*Manifest, bool, error) {
	if !ms.bigPrimary(ctx, deployment, backupID) {
		return ms.ReadIncludingTombstoned(ctx, deployment, backupID, verifier)
	}
	if verifier == nil {
		return nil, false, errors.New("backup: ReadIncludingTombstonedAuto requires a verifier")
	}
	dead, err := ms.IsTombstoned(ctx, deployment, backupID)
	if err != nil {
		return nil, false, fmt.Errorf("backup: tombstone check %s: %w", backupID, err)
	}
	info, err := ms.streamFilesT(ctx, deployment, backupID, StreamOptions{Verifier: verifier}, true, nil, true)
	if err != nil {
		return nil, false, err
	}
	m := info.Manifest
	m.stream = &fileStream{
		ms: ms, key: info.Key, sha: info.RawSHA256,
		files: info.Files, bytes: info.Bytes, chunkRefs: info.ChunkRefs,
	}
	return m, dead, nil
}

// bigPrimary reports whether the primary manifest object is over the
// threshold. Any Stat failure answers false: the held reader then
// produces the real error (or its replica fallback).
func (ms *ManifestStore) bigPrimary(ctx context.Context, deployment, backupID string) bool {
	st, err := ms.sp.Stat(ctx, PrimaryPath(deployment, backupID))
	if err != nil {
		// The held reader would fall back to the replica; if that is
		// the oversized copy (primary lost), it must be streamed too.
		rst, rerr := ms.sp.Stat(ctx, ReplicaPath(backupID))
		return rerr == nil && rst.Size > AutoStreamThreshold
	}
	return st.Size > AutoStreamThreshold
}

// ListAuto is List with per-manifest shape selection (see ReadAuto).
func (ms *ManifestStore) ListAuto(ctx context.Context, deployment string, verifier *Verifier) iter.Seq2[*Manifest, error] {
	return func(yield func(*Manifest, error) bool) {
		for e, err := range ms.listStreamingEntries(ctx, deployment, StreamOptions{Verifier: verifier}, verifier != nil, false, true) {
			if !yield(e.Manifest, err) {
				return
			}
		}
	}
}

// ListHeaders is List for callers that need only each manifest's header
// fields (ID, type, parent, timestamps, encryption...), such as retention:
// every manifest is verified as a stream and yielded header-only
// (Files == nil, entries reachable through EachFile), however small. A
// caller that collects the result holds N headers, not N manifests: List
// and ListAuto keep every manifest up to AutoStreamThreshold (64 MiB) whole.
func (ms *ManifestStore) ListHeaders(ctx context.Context, deployment string, verifier *Verifier) iter.Seq2[*Manifest, error] {
	return func(yield func(*Manifest, error) bool) {
		for e, err := range ms.listStreamingEntries(ctx, deployment, StreamOptions{Verifier: verifier}, verifier != nil, false, false) {
			if !yield(e.Manifest, err) {
				return
			}
		}
	}
}

// ListIncludingTombstonedHeaders is ListHeaders that also yields the
// soft-deleted manifests, flagged: header-only readers (status, the gap
// janitor) that must see the tombstoned set too.
func (ms *ManifestStore) ListIncludingTombstonedHeaders(ctx context.Context, deployment string, verifier *Verifier) iter.Seq2[ManifestEntry, error] {
	return ms.listStreamingEntries(ctx, deployment, StreamOptions{Verifier: verifier}, verifier != nil, true, false)
}

// ListAttestationlessHeaders is ListHeaders without signature
// verification, for read-only inspection that needs header fields only.
func (ms *ManifestStore) ListAttestationlessHeaders(ctx context.Context, deployment string) iter.Seq2[*Manifest, error] {
	return func(yield func(*Manifest, error) bool) {
		for e, err := range ms.listStreamingEntries(ctx, deployment, StreamOptions{}, true, false, false) {
			if !yield(e.Manifest, err) {
				return
			}
		}
	}
}

// ListAttestationlessIncludingTombstonedHeaders is
// ListAttestationlessHeaders that also yields soft-deleted manifests,
// flagged.
func (ms *ManifestStore) ListAttestationlessIncludingTombstonedHeaders(ctx context.Context, deployment string) iter.Seq2[ManifestEntry, error] {
	return ms.listStreamingEntries(ctx, deployment, StreamOptions{}, true, true, false)
}

// ListAttestationlessAuto is ListAuto without signature verification.
func (ms *ManifestStore) ListAttestationlessAuto(ctx context.Context, deployment string) iter.Seq2[*Manifest, error] {
	return func(yield func(*Manifest, error) bool) {
		for e, err := range ms.listStreamingEntries(ctx, deployment, StreamOptions{}, true, false, true) {
			if !yield(e.Manifest, err) {
				return
			}
		}
	}
}

// ListIncludingTombstonedAuto is ListAuto that also yields soft-deleted
// manifests, flagged.
func (ms *ManifestStore) ListIncludingTombstonedAuto(ctx context.Context, deployment string, verifier *Verifier) iter.Seq2[ManifestEntry, error] {
	return ms.listStreamingEntries(ctx, deployment, StreamOptions{Verifier: verifier}, verifier != nil, true, true)
}

// readHeld reads one small object into a held manifest, verifying per
// opts (the same parse List/ListAttestationless use).
func (ms *ManifestStore) readHeld(ctx context.Context, key string, opts StreamOptions) (*Manifest, error) {
	if opts.Verifier != nil {
		return ms.readVerified(ctx, key, opts.Verifier)
	}
	rc, err := ms.sp.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := ReadAllLimited(rc, MaxManifestBytes)
	if err != nil {
		return nil, err
	}
	return ParseAttestationless(body)
}
