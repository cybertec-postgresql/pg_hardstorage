package bundle_test

import (
	"archive/tar"
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/bundle"
)

// tarWithChunkEntries builds a tar of n regular content-addressed chunk
// entries, each `size` bytes. Each chunk lives at its canonical
// chunks/sha256/... key so it passes Import's payload-hash verification and
// imports cleanly (putIfNotExists) — leaving only the whole-bundle caps as
// the mechanism that can stop the import.
func tarWithChunkEntries(t *testing.T, n, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i := 0; i < n; i++ {
		// Distinct bytes per entry so keys differ; each key is the
		// content-address of its own body.
		body := append(bytes.Repeat([]byte("a"), size-1), byte(i))
		if size == 0 {
			body = nil
		}
		key := repo.ChunkKey(repo.HashOf(body))
		envelope := compression.WriteEnvelope(compression.AlgoNone, compression.EncryptionFields{}, body)
		if err := tw.WriteHeader(&tar.Header{
			Name:     key,
			Typeflag: tar.TypeReg,
			Size:     int64(len(envelope)),
			Mode:     0o644,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(envelope); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestImport_EntryCountCap pins input-validation audit #4: a bundle with
// more entries than the configured ceiling is refused, instead of doing
// unbounded per-entry work.
func TestImport_EntryCountCap(t *testing.T) {
	data := tarWithChunkEntries(t, 5, 4)
	_, err := bundle.Import(context.Background(), bytes.NewReader(data), newRepo(t),
		bundle.ImportOptions{MaxEntries: 3})
	if err == nil {
		t.Fatal("import of a too-many-entries bundle must be refused; got nil")
	}
	if !strings.Contains(err.Error(), "entry count") {
		t.Errorf("expected an entry-count cap error; got %v", err)
	}
}

// TestImport_TotalBytesCap: a bundle whose aggregate size exceeds the
// ceiling is refused even though each entry is individually small.
func TestImport_TotalBytesCap(t *testing.T) {
	data := tarWithChunkEntries(t, 4, 100) // 400 bytes aggregate
	_, err := bundle.Import(context.Background(), bytes.NewReader(data), newRepo(t),
		bundle.ImportOptions{MaxTotalBytes: 150})
	if err == nil {
		t.Fatal("import of an oversized-aggregate bundle must be refused; got nil")
	}
	if !strings.Contains(err.Error(), "total size") {
		t.Errorf("expected a total-size cap error; got %v", err)
	}
}

// TestImport_UnderCapsDoesNotTrip: a small input within both ceilings is
// not rejected by the caps. (The chunk-only tar still errors later for
// lacking bundle.json — but NOT due to a cap, which is what we assert.
// The full happy path is covered by TestExportImport_RoundTrip, which
// imports a real bundle under the default ceilings.)
func TestImport_UnderCapsDoesNotTrip(t *testing.T) {
	data := tarWithChunkEntries(t, 3, 50)
	_, err := bundle.Import(context.Background(), bytes.NewReader(data), newRepo(t),
		bundle.ImportOptions{MaxEntries: 100, MaxTotalBytes: 1 << 20})
	if err != nil && (strings.Contains(err.Error(), "entry count") || strings.Contains(err.Error(), "total size")) {
		t.Fatalf("under-cap import must not trip a cap; got %v", err)
	}
}
