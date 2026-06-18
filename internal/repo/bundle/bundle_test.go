package bundle_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/bundle"
)

func readAll(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// tar_NewWriter / tar_Header are tiny aliases so the
// schema-rejection test reads cleanly without overlap with the
// imported tar package.
var (
	tar_NewWriter = tar.NewWriter
)

type tar_Header = tar.Header

// newRepo opens a fresh fs-backed repo at a tempdir.
func newRepo(t *testing.T) storage.StoragePlugin {
	t.Helper()
	dir := t.TempDir()
	u, err := url.Parse("file://" + dir)
	if err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatalf("sp.Open: %v", err)
	}
	t.Cleanup(func() { sp.Close() })
	return sp
}

// putChunk writes one chunk and returns its hash + size.
func putChunk(t *testing.T, sp storage.StoragePlugin, body []byte) (repo.Hash, int64) {
	t.Helper()
	hash := repo.HashOf(body)
	key := repo.ChunkKey(hash)
	_, err := sp.Put(context.Background(), key, bytes.NewReader(body), storage.PutOptions{IfNotExists: true})
	if err != nil {
		t.Fatalf("put chunk: %v", err)
	}
	return hash, int64(len(body))
}

// commitManifest writes a manifest under its primary key.  We
// don't use ManifestStore.Commit (which would also write a
// replica + attestation) — for the bundle test we want
// fine-grained control over what's present in the source repo.
func commitManifest(t *testing.T, sp storage.StoragePlugin, m *backup.Manifest) {
	t.Helper()
	canonical, err := m.Canonicalize()
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	key := backup.PrimaryPath(m.Deployment, m.BackupID)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(canonical), storage.PutOptions{IfNotExists: true}); err != nil {
		t.Fatalf("put manifest: %v", err)
	}
}

func sampleManifest(t *testing.T, sp storage.StoragePlugin, backupID string) *backup.Manifest {
	t.Helper()
	h1, _ := putChunk(t, sp, []byte("chunk-alpha-bytes"))
	h2, _ := putChunk(t, sp, []byte("chunk-beta-bytes"))
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
		Compression:      "none",
		Files: []backup.FileEntry{
			{
				Path: "base/16384/2619",
				Size: 8192,
				Chunks: []backup.ChunkRef{
					{Hash: h1, Offset: 0, Len: 4096},
					{Hash: h2, Offset: 4096, Len: 4096},
				},
			},
		},
		WALRequired: []string{"000000010000000000000003"},
	}
}

func TestExportImport_RoundTrip(t *testing.T) {
	src := newRepo(t)
	m := sampleManifest(t, src, "db1.full.20260428T1200Z")
	commitManifest(t, src, m)

	var buf bytes.Buffer
	exported, err := bundle.Export(context.Background(), src, &buf, bundle.ExportOptions{
		Deployment:    "db1",
		BackupID:      m.BackupID,
		SourceRepoURL: "file:///source",
	})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if exported.ChunkCount != 2 {
		t.Errorf("expected 2 chunks in bundle, got %d", exported.ChunkCount)
	}
	if exported.ChunkBytes == 0 {
		t.Error("bundle.json should report non-zero chunk_bytes")
	}
	if got := exported.SourceRepo; got != "file:///source" {
		t.Errorf("source_repo = %q", got)
	}
	if len(exported.Backups) != 1 || exported.Backups[0].BackupID != m.BackupID {
		t.Errorf("backups = %#v", exported.Backups)
	}

	dst := newRepo(t)
	imported, err := bundle.Import(context.Background(), &buf, dst, bundle.ImportOptions{})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if imported.ChunkCount != 2 {
		t.Errorf("imported bundle: ChunkCount=%d", imported.ChunkCount)
	}

	// Destination repo: verify the manifest body is there
	// (unsigned, since the bundle preserves the source's
	// signing posture).  Use ParseAttestationless so the
	// assertion doesn't depend on a verifier the test would
	// have to wire up.
	manifestKey := backup.PrimaryPath("db1", m.BackupID)
	rc, err := dst.Get(context.Background(), manifestKey)
	if err != nil {
		t.Fatalf("manifest absent in dst: %v", err)
	}
	body := readAll(t, rc)
	got, err := backup.ParseAttestationless(body)
	if err != nil {
		t.Fatalf("parse imported manifest: %v", err)
	}
	if got.BackupID != m.BackupID {
		t.Errorf("imported manifest BackupID = %q", got.BackupID)
	}
	if len(got.Files) != 1 || len(got.Files[0].Chunks) != 2 {
		t.Errorf("imported manifest files = %#v", got.Files)
	}
	for _, ref := range got.Files[0].Chunks {
		if _, err := dst.Stat(context.Background(), repo.ChunkKey(ref.Hash)); err != nil {
			t.Errorf("chunk %s missing in destination: %v", ref.Hash, err)
		}
	}
}

// TestImport_VerifierRejectsWrongKey pins ImportOptions.Verifier:
// when set, Import must run each signed backup manifest through
// ParseAndVerify after ingest (the destination-side strict-signing
// posture the package documents).  A bundle whose manifest is signed
// by key A must be rejected under a verifier pinned to a different
// key B, accepted under A's verifier, and ingested unverified when no
// verifier is supplied.
func TestImport_VerifierRejectsWrongKey(t *testing.T) {
	privA, pubA, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatalf("keypair A: %v", err)
	}
	_, pubB, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatalf("keypair B: %v", err)
	}
	signerA, err := backup.LoadSigner(privA)
	if err != nil {
		t.Fatalf("signer A: %v", err)
	}
	verifierA, err := backup.LoadVerifier(pubA)
	if err != nil {
		t.Fatalf("verifier A: %v", err)
	}
	verifierB, err := backup.LoadVerifier(pubB)
	if err != nil {
		t.Fatalf("verifier B: %v", err)
	}

	// makeBundle builds a fresh source repo, signs the manifest with
	// A and writes it to the primary path exactly as
	// ManifestStore.Commit would (MarshalToBytes carries the inline
	// attestation), then exports a bundle.  Returns the bundle bytes.
	makeBundle := func(t *testing.T) []byte {
		t.Helper()
		src := newRepo(t)
		m := sampleManifest(t, src, "db1.full.20260428T1400Z")
		if err := m.Sign(signerA); err != nil {
			t.Fatalf("sign: %v", err)
		}
		body, err := m.MarshalToBytes()
		if err != nil {
			t.Fatalf("marshal signed manifest: %v", err)
		}
		key := backup.PrimaryPath(m.Deployment, m.BackupID)
		if _, err := src.Put(context.Background(), key, bytes.NewReader(body), storage.PutOptions{IfNotExists: true}); err != nil {
			t.Fatalf("put signed manifest: %v", err)
		}
		var buf bytes.Buffer
		if _, err := bundle.Export(context.Background(), src, &buf, bundle.ExportOptions{
			Deployment: "db1", BackupID: m.BackupID,
		}); err != nil {
			t.Fatalf("Export: %v", err)
		}
		return buf.Bytes()
	}

	// Wrong trusted key: must be rejected before the manifest lands.
	_, err = bundle.Import(context.Background(), bytes.NewReader(makeBundle(t)), newRepo(t),
		bundle.ImportOptions{Verifier: verifierB})
	if err == nil {
		t.Fatal("Import with wrong verifier key should be rejected")
	}
	if !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("expected signature verification failure, got %v", err)
	}

	// Correct key: accepted.
	if _, err := bundle.Import(context.Background(), bytes.NewReader(makeBundle(t)), newRepo(t),
		bundle.ImportOptions{Verifier: verifierA}); err != nil {
		t.Fatalf("Import with correct verifier key should succeed: %v", err)
	}

	// No verifier: ingested without signature checking.
	if _, err := bundle.Import(context.Background(), bytes.NewReader(makeBundle(t)), newRepo(t),
		bundle.ImportOptions{}); err != nil {
		t.Fatalf("Import with no verifier should succeed: %v", err)
	}
}

func TestExport_RefusesEmptyDeployment(t *testing.T) {
	src := newRepo(t)
	var buf bytes.Buffer
	_, err := bundle.Export(context.Background(), src, &buf, bundle.ExportOptions{})
	if err == nil || !strings.Contains(err.Error(), "Deployment") {
		t.Errorf("expected Deployment-required error, got %v", err)
	}
}

func TestExport_NoBackupsRefuses(t *testing.T) {
	src := newRepo(t)
	var buf bytes.Buffer
	_, err := bundle.Export(context.Background(), src, &buf, bundle.ExportOptions{
		Deployment: "db-empty",
	})
	if err == nil || !strings.Contains(err.Error(), "no manifests") {
		t.Errorf("expected no-manifests error, got %v", err)
	}
}

func TestImport_IsIdempotent(t *testing.T) {
	src := newRepo(t)
	m := sampleManifest(t, src, "db1.full.20260428T1300Z")
	commitManifest(t, src, m)

	var buf bytes.Buffer
	if _, err := bundle.Export(context.Background(), src, &buf, bundle.ExportOptions{
		Deployment: "db1", BackupID: m.BackupID,
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	bundleBytes := buf.Bytes()

	dst := newRepo(t)
	if _, err := bundle.Import(context.Background(), bytes.NewReader(bundleBytes), dst, bundle.ImportOptions{}); err != nil {
		t.Fatalf("first Import: %v", err)
	}
	// Second import should succeed silently.
	if _, err := bundle.Import(context.Background(), bytes.NewReader(bundleBytes), dst, bundle.ImportOptions{}); err != nil {
		t.Fatalf("second Import (idempotency): %v", err)
	}
}

// TestImport_RejectsPathTraversal asserts the audit-v26
// finding (path traversal) stays fixed: a malicious bundle
// containing entries with ".." or absolute paths must be
// refused before reaching storage.Put.
func TestImport_RejectsPathTraversal(t *testing.T) {
	cases := []string{
		"../etc/passwd",
		"chunks/../../../etc/passwd",
		"/absolute/path",
		"./relative/with/dot",
	}
	for _, name := range cases {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		body := []byte("malicious")
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
		tw.Close()

		dst := newRepo(t)
		_, err := bundle.Import(context.Background(), &buf, dst, bundle.ImportOptions{})
		if err == nil {
			t.Errorf("entry %q should be refused", name)
		} else if !strings.Contains(err.Error(), "path traversal") {
			t.Errorf("entry %q: expected path-traversal refusal; got %v", name, err)
		}
	}
}

// TestImport_RejectsOversizedEntry asserts the audit-v26
// finding (tar size DoS) stays fixed: an entry declaring a
// size larger than MaxEntryBytes is refused before reading
// the body into memory.
func TestImport_RejectsOversizedEntry(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// Declare an oversized entry — we don't actually write
	// the body (the importer should reject before reading).
	if err := tw.WriteHeader(&tar.Header{
		Name: "chunks/sha256/aa/bb/aabbcc.chk",
		Mode: 0o644,
		Size: bundle.MaxEntryBytes + 1,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Close()

	dst := newRepo(t)
	_, err := bundle.Import(context.Background(), &buf, dst, bundle.ImportOptions{})
	if err == nil {
		t.Fatal("oversized entry should be refused")
	}
	if !strings.Contains(err.Error(), "MaxEntryBytes") {
		t.Errorf("expected MaxEntryBytes refusal; got %v", err)
	}
}

func TestImport_RejectsBadSchema(t *testing.T) {
	var buf bytes.Buffer
	tw := tar_NewWriter(&buf)
	bad := []byte(`{"schema":"some.other.schema.v9"}`)
	if err := tw.WriteHeader(&tar_Header{Name: "bundle.json", Mode: 0o644, Size: int64(len(bad))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(bad); err != nil {
		t.Fatal(err)
	}
	tw.Close()

	dst := newRepo(t)
	_, err := bundle.Import(context.Background(), &buf, dst, bundle.ImportOptions{})
	if err == nil || !strings.Contains(err.Error(), "unsupported bundle schema") {
		t.Errorf("expected schema rejection, got %v", err)
	}
}
