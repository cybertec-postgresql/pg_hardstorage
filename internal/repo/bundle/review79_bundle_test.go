package bundle_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/bundle"
)

// writeTar builds a tar from a map of name→body in a stable order.
func writeTar(t *testing.T, entries []struct{ name, body string }) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name:     e.name,
			Typeflag: tar.TypeReg,
			Size:     int64(len(e.body)),
			Mode:     0o644,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestImport_RejectsChunkHashMismatch is the regression guard for bug 19:
// Import must verify each chunk payload's SHA-256 against the
// content-addressed key before writing. A bundle that ships a chunk whose
// bytes don't hash to its key (corrupt or malicious) must be rejected and
// nothing planted in the CAS.
func TestImport_RejectsChunkHashMismatch(t *testing.T) {
	// Key addresses the hash of "honest-payload" ...
	honest := []byte("honest-payload")
	key := repo.ChunkKey(repo.HashOf(honest))
	// ... but the bundle ships DIFFERENT bytes at that key.
	forged := "totally-different-bytes"

	bundleJSON := `{"schema":"pg_hardstorage.repobundle.v1"}`
	data := writeTar(t, []struct{ name, body string }{
		{"bundle.json", bundleJSON},
		{key, forged},
	})

	dst := newRepo(t)
	_, err := bundle.Import(context.Background(), bytes.NewReader(data), dst, bundle.ImportOptions{})
	if err == nil {
		t.Fatal("import of a chunk whose payload doesn't match its key must be rejected; got nil")
	}
	if !strings.Contains(err.Error(), "does not match key hash") {
		t.Errorf("expected a hash-mismatch rejection; got %v", err)
	}
	// The forged chunk must NOT have landed in the CAS.
	if _, serr := dst.Stat(context.Background(), key); serr == nil {
		t.Error("forged chunk was written to the repo despite hash mismatch")
	} else if !errors.Is(serr, storage.ErrNotFound) {
		t.Errorf("unexpected Stat error: %v", serr)
	}
}

// TestImport_AcceptsMatchingChunkHash confirms the fix doesn't reject an
// honest, correctly-addressed chunk.
func TestImport_AcceptsMatchingChunkHash(t *testing.T) {
	honest := "honest-payload"
	key := repo.ChunkKey(repo.HashOf([]byte(honest)))
	bundleJSON := `{"schema":"pg_hardstorage.repobundle.v1"}`
	data := writeTar(t, []struct{ name, body string }{
		{"bundle.json", bundleJSON},
		{key, honest},
	})

	dst := newRepo(t)
	if _, err := bundle.Import(context.Background(), bytes.NewReader(data), dst, bundle.ImportOptions{}); err != nil {
		t.Fatalf("import of an honest chunk must succeed; got %v", err)
	}
	if _, err := dst.Stat(context.Background(), key); err != nil {
		t.Errorf("honest chunk should be present after import: %v", err)
	}
}

// TestExportImport_WALRoundTrip is the regression guard for bug 36:
// walSegmentKey must build wal/<dep>/<8hex-TLI>/<seg>.json (matching
// walsink.SegmentPath), not the old decimal-timeline/no-suffix form. With
// the wrong key, Export couldn't find the WAL manifest and import planted
// nothing; this proves a WAL segment manifest round-trips.
func TestExportImport_WALRoundTrip(t *testing.T) {
	src := newRepo(t)
	m := sampleManifest(t, src, "db1.full.20260428T1200Z")
	seg := m.WALRequired[0] // "000000010000000000000003"
	commitManifest(t, src, m)

	// Plant the WAL segment manifest at the CORRECT layout:
	//   wal/<dep>/<TIMELINE-8-hex>/<seg>.json     (timeline 1 -> 00000001)
	walKey := "wal/db1/00000001/" + seg + ".json"
	walBody := `{"schema":"pg_hardstorage.wal_segment.v1"}`
	if _, err := src.Put(context.Background(), walKey, bytes.NewReader([]byte(walBody)),
		storage.PutOptions{ContentLength: int64(len(walBody))}); err != nil {
		t.Fatalf("plant wal manifest: %v", err)
	}

	var buf bytes.Buffer
	exported, err := bundle.Export(context.Background(), src, &buf, bundle.ExportOptions{
		Deployment:    "db1",
		BackupID:      m.BackupID,
		SourceRepoURL: "file:///source",
		IncludeWAL:    true,
	})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(exported.WAL) != 1 {
		t.Fatalf("expected 1 WAL entry in bundle, got %d (%+v)", len(exported.WAL), exported.WAL)
	}

	dst := newRepo(t)
	if _, err := bundle.Import(context.Background(), &buf, dst, bundle.ImportOptions{}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	// The WAL segment manifest must land at the same correct key at dst.
	if _, err := dst.Stat(context.Background(), walKey); err != nil {
		t.Errorf("WAL segment manifest missing at dst key %q: %v", walKey, err)
	}
}
