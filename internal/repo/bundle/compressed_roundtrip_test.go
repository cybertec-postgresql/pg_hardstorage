package bundle_test

// compressed_roundtrip_test.go — a bundle must import chunks written
// the way real repositories write them.
//
// Every other test here writes chunks straight to storage as raw
// plaintext under ChunkKey(HashOf(body)). That is the ONE arrangement
// in which stored bytes hash to the key, and no repo stores anything
// that way: CAS.PutChunk hashes the PLAINTEXT, then compresses (zstd
// by default) and optionally encrypts before writing. Export preserves
// the on-disk layout exactly, so a real bundle carries compressed
// bytes under a plaintext-addressed key.
//
// Bypassing the CAS hid a total failure of the feature: import hashed
// the compressed bytes, compared them to the plaintext address, and
// refused every chunk of every real bundle —
//
//	bundle: reject chunk chunks/sha256/...: payload SHA-256 <x> does
//	not match key hash <y>
//
// Caught by L2_repo_maintenance and L2_repo_replication, two of the
// 162 scenarios that no make target runs.

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression/none"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression/zstd"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/bundle"
)

func realCodecs() *compression.CodecRegistry {
	r := compression.NewRegistry()
	r.Register(compression.AlgoNone, none.Compressor{})
	r.Register(compression.AlgoZstd, zstd.NewDefault())
	return r
}

// compressibleManifest writes two chunks through a real CAS and
// returns a manifest referencing them. It asserts the codec actually
// engaged, so the test cannot silently decay into the raw-plaintext
// shape it exists to escape.
func compressibleManifest(t *testing.T, sp storageGetter, backupID string) *backup.Manifest {
	t.Helper()
	cas := repo.NewCAS(sp, repo.WithCompressor(zstd.NewDefault()))

	b1 := bytes.Repeat([]byte("alpha compressible payload "), 512)
	b2 := bytes.Repeat([]byte("beta compressible payload "), 512)
	i1, err := cas.PutChunk(context.Background(), b1)
	if err != nil {
		t.Fatalf("PutChunk: %v", err)
	}
	i2, err := cas.PutChunk(context.Background(), b2)
	if err != nil {
		t.Fatalf("PutChunk: %v", err)
	}
	for _, h := range []repo.Hash{i1.Hash, i2.Hash} {
		stored := readAll(t, mustGet(t, sp, repo.ChunkKey(h)))
		if repo.HashOf(stored) == h {
			t.Fatal("stored bytes hash to the key — the codec did not engage, " +
				"so this test would not exercise the compressed path at all")
		}
	}
	return &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         backupID,
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        170,
		SystemIdentifier: "7388123456789012345",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		StartedAt:        time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC),
		StoppedAt:        time.Date(2026, 4, 28, 12, 8, 23, 0, time.UTC),
		Compression:      "zstd",
		Files: []backup.FileEntry{{
			Path: "base/16384/2619",
			Size: int64(len(b1) + len(b2)),
			Chunks: []backup.ChunkRef{
				{Hash: i1.Hash, Offset: 0, Len: int64(len(b1))},
				{Hash: i2.Hash, Offset: int64(len(b1)), Len: int64(len(b2))},
			},
		}},
	}
}

func TestExportImport_RoundTripCompressedChunks(t *testing.T) {
	src := newRepo(t)
	m := compressibleManifest(t, src, "db1.full.20260428T1200Z")
	commitManifest(t, src, m)

	var buf bytes.Buffer
	if _, err := bundle.Export(context.Background(), src, &buf, bundle.ExportOptions{
		Deployment: "db1", BackupID: m.BackupID, SourceRepoURL: "file:///source",
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	dst := newRepo(t)
	if _, err := bundle.Import(context.Background(), &buf, dst,
		bundle.ImportOptions{Codecs: realCodecs()}); err != nil {
		t.Fatalf("import refused chunks written by a real CAS: %v", err)
	}
	for _, ref := range m.Files[0].Chunks {
		if _, err := dst.Stat(context.Background(), repo.ChunkKey(ref.Hash)); err != nil {
			t.Errorf("chunk %s missing in destination: %v", ref.Hash, err)
		}
	}
}

// The guarantee must survive the fix: a chunk whose PLAINTEXT does not
// hash to its key is still refused, or the CAS would serve wrong
// content to every later reader.
func TestImport_RejectsForgedCompressedChunk(t *testing.T) {
	src := newRepo(t)
	m := compressibleManifest(t, src, "db1.full.20260428T1200Z")
	commitManifest(t, src, m)

	var buf bytes.Buffer
	if _, err := bundle.Export(context.Background(), src, &buf, bundle.ExportOptions{
		Deployment: "db1", BackupID: m.BackupID, SourceRepoURL: "file:///source",
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Swap one chunk's body for a different, validly-compressed payload
	// while leaving its key alone.
	forged := bytes.Repeat([]byte("forged payload "), 512)
	payload, algo, cerr := zstd.NewDefault().Compress(forged)
	if cerr != nil {
		t.Fatalf("compress: %v", cerr)
	}
	tampered := retarWithChunkBody(t, buf.Bytes(),
		repo.ChunkKey(m.Files[0].Chunks[0].Hash),
		compression.WriteEnvelope(algo, compression.EncryptionFields{}, payload))

	dst := newRepo(t)
	_, err := bundle.Import(context.Background(), bytes.NewReader(tampered), dst,
		bundle.ImportOptions{Codecs: realCodecs()})
	if err == nil {
		t.Fatal("a chunk whose plaintext does not hash to its key was accepted")
	}
	t.Logf("forged chunk refused: %v", err)
}

// storageGetter is the slice of StoragePlugin these helpers need.
type storageGetter = storage.StoragePlugin

func mustGet(t *testing.T, sp storage.StoragePlugin, key string) io.ReadCloser {
	t.Helper()
	rc, err := sp.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return rc
}

// retarWithChunkBody rewrites a bundle tar, replacing the body of one
// chunk entry and leaving every other entry byte-identical.
func retarWithChunkBody(t *testing.T, tarball []byte, key string, body []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	tr := tar.NewReader(bytes.NewReader(tarball))
	replaced := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		payload, rerr := io.ReadAll(tr)
		if rerr != nil {
			t.Fatalf("read entry %s: %v", hdr.Name, rerr)
		}
		if strings.TrimPrefix(hdr.Name, "./") == key {
			payload = body
			replaced = true
		}
		nh := *hdr
		nh.Size = int64(len(payload))
		if err := tw.WriteHeader(&nh); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write(payload); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if !replaced {
		t.Fatalf("chunk %s not found in bundle — test would be vacuous", key)
	}
	return out.Bytes()
}
