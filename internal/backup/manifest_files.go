package backup

import (
	"context"
	"errors"
	"fmt"
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
