package restore

// materialize_internal_test.go — direct coverage of materializeFile,
// the function that rebuilds every backed-up file from its chunks. It
// was exercised only through integration boots (well-formed manifests
// only), never directly, on the single most consequential surface in
// the system: "the restored bytes equal the backed-up bytes."
//
// The specific probe: materializeFile writes chunks in slice order and
// (before this pass) did not read ref.Offset — so a reordered chunk
// list that still summed to the right size would restore
// byte-scrambled data that passes every check the function makes. Safe
// in production only because Manifest.Validate enforces contiguity and
// restore re-runs it first; but the function is one removed Validate
// call away from silent corruption, and had nothing pinning that.

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func materializeTestCAS(t *testing.T) *repo.CAS {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return repo.NewCAS(sp)
}

// chunkOf puts body and returns a ChunkRef at the given offset.
func chunkOf(t *testing.T, cas *repo.CAS, off int64, body []byte) backup.ChunkRef {
	t.Helper()
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	return backup.ChunkRef{Hash: info.Hash, Offset: off, Len: info.Size}
}

// TestMaterializeFile_MultiChunkRoundTrip: the happy path, direct —
// several chunks reassemble to exactly the original bytes.
func TestMaterializeFile_MultiChunkRoundTrip(t *testing.T) {
	cas := materializeTestCAS(t)
	a, b, c := []byte("AAAAA"), []byte("BBBBBBB"), []byte("CCC")
	f := &backup.FileEntry{
		Path: "base/16384/2619", Mode: 0o600,
		Size:   int64(len(a) + len(b) + len(c)),
		Chunks: []backup.ChunkRef{chunkOf(t, cas, 0, a), chunkOf(t, cas, 5, b), chunkOf(t, cas, 12, c)},
	}
	dest := t.TempDir()
	n, _, err := materializeFile(context.Background(), cas, dest, f)
	if err != nil {
		t.Fatalf("materializeFile: %v", err)
	}
	if n != f.Size {
		t.Fatalf("wrote %d bytes, want %d", n, f.Size)
	}
	got, err := os.ReadFile(filepath.Join(dest, f.Path))
	if err != nil {
		t.Fatal(err)
	}
	if want := "AAAAABBBBBBBCCC"; string(got) != want {
		t.Fatalf("reassembled %q, want %q", got, want)
	}
}

// TestMaterializeFile_ReorderedChunksDetected is the finding. A chunk
// list whose offsets are NOT ascending-contiguous — but which still
// sums to the right size — must not silently produce scrambled bytes.
// Before the offset check materializeFile wrote them in slice order
// and passed the size check, yielding a corrupt file with no error.
func TestMaterializeFile_ReorderedChunksDetected(t *testing.T) {
	cas := materializeTestCAS(t)
	a, b := []byte("AAAAA"), []byte("BBBBBBB")
	// Offsets claim a@0 b@5 (contiguous), but the SLICE is reversed:
	// concatenating slice order gives "BBBBBBBAAAAA" while the offsets
	// say "AAAAABBBBBBB". Same total size — the size check can't see it.
	f := &backup.FileEntry{
		Path: "base/16384/2619", Mode: 0o600,
		Size: int64(len(a) + len(b)),
		Chunks: []backup.ChunkRef{
			chunkOf(t, cas, 5, b), // out of order: offset 5 first
			chunkOf(t, cas, 0, a),
		},
	}
	dest := t.TempDir()
	_, _, err := materializeFile(context.Background(), cas, dest, f)
	if err == nil {
		got, _ := os.ReadFile(filepath.Join(dest, f.Path))
		t.Fatalf("materializeFile accepted a reordered chunk list and wrote %q — a "+
			"byte-scrambled file that passes the size check is silent restore corruption; "+
			"materializeFile must verify ref.Offset matches the running write position", got)
	}
}
