package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// bundleWorld is a self-contained fixture: an init'd repo + an
// audit Store + a signer for bundle signing.
type bundleWorld struct {
	sp      storage.StoragePlugin
	store   *audit.Store
	log     *audit.StorageBackedLog
	signer  audit.EventSigner
	repoURL string
}

// signerAdapter wraps backup.Signer to satisfy audit.EventSigner.
// Same shape; the interface is on the audit side to avoid the
// import cycle (audit can't import backup).
type signerAdapter struct {
	s *backup.Signer
}

func (a signerAdapter) Sign(payload []byte) []byte    { return a.s.Sign(payload) }
func (a signerAdapter) PublicKey() ed25519.PublicKey  { return a.s.PublicKey() }
func (a signerAdapter) PublicKeyPEM() ([]byte, error) { return a.s.PublicKeyPEM() }

func setupBundleWorld(t *testing.T) *bundleWorld {
	t.Helper()
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	priv, _, _ := backup.GenerateKeypair(rand.Reader)
	bsigner, _ := backup.LoadSigner(priv)
	return &bundleWorld{
		sp:      sp,
		store:   audit.NewStore(sp),
		log:     audit.NewStorageBackedLog(sp),
		signer:  signerAdapter{s: bsigner},
		repoURL: repoURL,
	}
}

// appendEvent plants one event in the audit chain.
func (w *bundleWorld) appendEvent(t *testing.T, action, deployment string, at time.Time) {
	t.Helper()
	ev := &audit.Event{
		Action:    action,
		Subject:   audit.Subject{Deployment: deployment},
		Timestamp: at,
	}
	if err := w.store.Append(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
}

// TestExportBundle_Validation
func TestExportBundle_Validation(t *testing.T) {
	w := setupBundleWorld(t)
	if _, err := audit.ExportBundle(context.Background(), nil, &bytes.Buffer{},
		w.signer, audit.ExportOptions{}); err == nil {
		t.Error("nil sp must error")
	}
	if _, err := audit.ExportBundle(context.Background(), w.sp, nil,
		w.signer, audit.ExportOptions{}); err == nil {
		t.Error("nil writer must error")
	}
	if _, err := audit.ExportBundle(context.Background(), w.sp, &bytes.Buffer{},
		nil, audit.ExportOptions{}); err == nil {
		t.Error("nil signer must error")
	}
}

// TestExportBundle_EmptyChain: a fresh repo produces a valid
// bundle with zero events.
func TestExportBundle_EmptyChain(t *testing.T) {
	w := setupBundleWorld(t)
	var buf bytes.Buffer
	res, err := audit.ExportBundle(context.Background(), w.sp, &buf, w.signer, audit.ExportOptions{
		SourceURL: w.repoURL,
		Operator:  "test",
	})
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	if res.EventCount != 0 {
		t.Errorf("EventCount = %d, want 0", res.EventCount)
	}
	if res.SHA256 == "" {
		t.Errorf("SHA256 should be set")
	}
	if res.BundleBytes <= 0 {
		t.Errorf("BundleBytes = %d, want > 0", res.BundleBytes)
	}
	if res.Manifest == nil {
		t.Errorf("Manifest should be set")
	}
}

// TestExportBundle_RoundTrip: bundle → verify → manifest.
func TestExportBundle_RoundTrip(t *testing.T) {
	w := setupBundleWorld(t)
	now := time.Now().UTC()
	w.appendEvent(t, "backup.create", "db1", now.Add(-2*time.Hour))
	w.appendEvent(t, "backup.create", "db1", now.Add(-1*time.Hour))
	w.appendEvent(t, "backup.delete", "db2", now.Add(-30*time.Minute))

	var buf bytes.Buffer
	res, err := audit.ExportBundle(context.Background(), w.sp, &buf, w.signer, audit.ExportOptions{
		SourceURL: w.repoURL,
		Operator:  "alice@example.com",
	})
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	if res.EventCount != 3 {
		t.Errorf("EventCount = %d, want 3", res.EventCount)
	}

	manifest, err := audit.VerifyBundle(&buf)
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if manifest.EventCount != 3 {
		t.Errorf("manifest.EventCount = %d", manifest.EventCount)
	}
	if manifest.Operator != "alice@example.com" {
		t.Errorf("Operator = %q", manifest.Operator)
	}
	if manifest.SourceURL != w.repoURL {
		t.Errorf("SourceURL = %q", manifest.SourceURL)
	}
	if manifest.SignatureAlgorithm != "ed25519" {
		t.Errorf("SignatureAlgorithm = %q", manifest.SignatureAlgorithm)
	}
}

// TestExportBundle_FilterByAction: only events matching the
// filter end up in the bundle.
func TestExportBundle_FilterByAction(t *testing.T) {
	w := setupBundleWorld(t)
	now := time.Now().UTC()
	w.appendEvent(t, "backup.create", "db1", now.Add(-2*time.Hour))
	w.appendEvent(t, "backup.delete", "db1", now.Add(-1*time.Hour))
	w.appendEvent(t, "kms.rotate", "", now.Add(-30*time.Minute))

	var buf bytes.Buffer
	res, err := audit.ExportBundle(context.Background(), w.sp, &buf, w.signer, audit.ExportOptions{
		Filters: audit.ListFilters{ActionPrefix: "backup."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.EventCount != 2 {
		t.Errorf("EventCount = %d, want 2 (backup.* only)", res.EventCount)
	}
	manifest, err := audit.VerifyBundle(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Filters.ActionPrefix != "backup." {
		t.Errorf("Filters.ActionPrefix = %q", manifest.Filters.ActionPrefix)
	}
}

// TestExportBundle_TimeWindow: Since/Until clip the events.
func TestExportBundle_TimeWindow(t *testing.T) {
	w := setupBundleWorld(t)
	now := time.Now().UTC()
	w.appendEvent(t, "x.event", "db1", now.Add(-3*time.Hour))
	w.appendEvent(t, "x.event", "db1", now.Add(-1*time.Hour))
	w.appendEvent(t, "x.event", "db1", now.Add(-30*time.Minute))

	var buf bytes.Buffer
	res, err := audit.ExportBundle(context.Background(), w.sp, &buf, w.signer, audit.ExportOptions{
		Filters: audit.ListFilters{
			Since: now.Add(-2 * time.Hour),
			Until: now.Add(-15 * time.Minute),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.EventCount != 2 {
		t.Errorf("windowed EventCount = %d, want 2", res.EventCount)
	}
}

// TestExportBundle_IncludeAnchors: opt-in dumps anchor history.
func TestExportBundle_IncludeAnchors(t *testing.T) {
	w := setupBundleWorld(t)
	w.appendEvent(t, "x.event", "db1", time.Now().UTC())
	// Plant an anchor.
	if _, err := w.log.PutAnchor(context.Background(), audit.Anchor{
		Schema:        audit.AnchorSchema,
		ChainHeadHash: "abc123",
		HeadSequence:  1,
		AnchoredAt:    time.Now().UTC(),
		PublisherID:   "test",
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	res, err := audit.ExportBundle(context.Background(), w.sp, &buf, w.signer, audit.ExportOptions{
		IncludeAnchors: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.AnchorCount != 1 {
		t.Errorf("AnchorCount = %d, want 1", res.AnchorCount)
	}

	manifest, err := audit.VerifyBundle(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.AnchorCount != 1 {
		t.Errorf("manifest.AnchorCount = %d", manifest.AnchorCount)
	}
	hasAnchorsFile := false
	for _, name := range manifest.SignedFiles {
		if name == "anchors.ndjson" {
			hasAnchorsFile = true
		}
	}
	if !hasAnchorsFile {
		t.Errorf("anchors.ndjson missing from signed_files: %v", manifest.SignedFiles)
	}
}

// TestExportBundle_NoAnchors_NoFile: without IncludeAnchors,
// anchors.ndjson is NOT in the bundle.
func TestExportBundle_NoAnchors_NoFile(t *testing.T) {
	w := setupBundleWorld(t)
	w.appendEvent(t, "x.event", "db1", time.Now().UTC())

	var buf bytes.Buffer
	if _, err := audit.ExportBundle(context.Background(), w.sp, &buf, w.signer, audit.ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	manifest, err := audit.VerifyBundle(&buf)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range manifest.SignedFiles {
		if name == "anchors.ndjson" {
			t.Errorf("unexpected anchors.ndjson in bundle: %v", manifest.SignedFiles)
		}
	}
}

// TestVerifyBundle_TamperedSignatureRefused: flipping a byte in
// the bundle must fail verification.
func TestVerifyBundle_TamperedSignatureRefused(t *testing.T) {
	w := setupBundleWorld(t)
	w.appendEvent(t, "x.event", "db1", time.Now().UTC())

	var buf bytes.Buffer
	if _, err := audit.ExportBundle(context.Background(), w.sp, &buf, w.signer, audit.ExportOptions{}); err != nil {
		t.Fatal(err)
	}

	// Flip a single byte deep in the gzipped tar.  Most
	// positions will produce a gzip CRC failure or tar parse
	// error; either is a verifier-rejection.  We pick the
	// middle byte.
	b := buf.Bytes()
	b[len(b)/2] ^= 0xFF
	if _, err := audit.VerifyBundle(bytes.NewReader(b)); err == nil {
		t.Error("tampered bundle verified clean; want rejection")
	}
}

// TestVerifyBundle_RejectsEmpty: a non-bundle blob is refused.
func TestVerifyBundle_RejectsEmpty(t *testing.T) {
	if _, err := audit.VerifyBundle(bytes.NewReader([]byte{0x1f, 0x8b})); err == nil {
		t.Error("non-bundle blob should error")
	}
}

// TestVerifyBundle_RejectsCorrupt: random bytes also rejected.
func TestVerifyBundle_RejectsCorrupt(t *testing.T) {
	if _, err := audit.VerifyBundle(bytes.NewReader([]byte("not a tarball"))); err == nil {
		t.Error("garbage should error")
	}
}

// TestExportBundle_ChainProof_HasEdgeEvents
func TestExportBundle_ChainProof_HasEdgeEvents(t *testing.T) {
	w := setupBundleWorld(t)
	now := time.Now().UTC()
	w.appendEvent(t, "x.event", "db1", now.Add(-2*time.Hour))
	w.appendEvent(t, "x.event", "db1", now.Add(-1*time.Hour))

	var buf bytes.Buffer
	if _, err := audit.ExportBundle(context.Background(), w.sp, &buf, w.signer, audit.ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	manifest, err := audit.VerifyBundle(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.HeadHash == "" {
		t.Errorf("HeadHash should be set")
	}
	if manifest.HeadSequence == 0 {
		t.Errorf("HeadSequence = 0; expected positive")
	}
}

// TestExportBundle_DeterministicSHA: building the same bundle
// from the same input twice (same Now) yields the same SHA-256.
func TestExportBundle_DeterministicSHA(t *testing.T) {
	w := setupBundleWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.appendEvent(t, "x.event", "db1", now.Add(-1*time.Hour))

	var buf1, buf2 bytes.Buffer
	res1, err := audit.ExportBundle(context.Background(), w.sp, &buf1, w.signer, audit.ExportOptions{
		Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	res2, err := audit.ExportBundle(context.Background(), w.sp, &buf2, w.signer, audit.ExportOptions{
		Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	// ed25519 signatures are deterministic for a given (key, message)
	// per RFC 8032; same input → same bundle bytes → same SHA.
	if res1.SHA256 != res2.SHA256 {
		t.Errorf("SHA mismatch across deterministic runs: %q vs %q",
			res1.SHA256, res2.SHA256)
	}
}

// TestExportBundle_ManifestSignedFiles_OrderStable: signed_files
// records the order the verifier will use.
func TestExportBundle_ManifestSignedFiles_OrderStable(t *testing.T) {
	w := setupBundleWorld(t)
	w.appendEvent(t, "x.event", "db1", time.Now().UTC())

	var buf bytes.Buffer
	if _, err := audit.ExportBundle(context.Background(), w.sp, &buf, w.signer, audit.ExportOptions{
		IncludeAnchors: false,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, err := audit.VerifyBundle(&buf)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"events.ndjson", "chain_proof.json", "public_key.pem", "README.md", "bundle.json"}
	if len(manifest.SignedFiles) != len(want) {
		t.Fatalf("signed_files = %v, want %v", manifest.SignedFiles, want)
	}
	for i, w := range want {
		if manifest.SignedFiles[i] != w {
			t.Errorf("signed_files[%d] = %q, want %q", i, manifest.SignedFiles[i], w)
		}
	}
}

// TestExportBundle_PublicKeyFingerprint
func TestExportBundle_PublicKeyFingerprint(t *testing.T) {
	w := setupBundleWorld(t)
	w.appendEvent(t, "x.event", "db1", time.Now().UTC())

	var buf bytes.Buffer
	if _, err := audit.ExportBundle(context.Background(), w.sp, &buf, w.signer, audit.ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	manifest, err := audit.VerifyBundle(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.PublicKeyFingerprint == "" {
		t.Errorf("PublicKeyFingerprint should be set")
	}
	// Format: 16 lowercase-hex chars.
	if len(manifest.PublicKeyFingerprint) != 16 {
		t.Errorf("fingerprint length = %d, want 16", len(manifest.PublicKeyFingerprint))
	}
}

// TestVerifyBundle_RejectsPathTraversal: a maliciously crafted
// tarball with .. in a header is rejected.
func TestVerifyBundle_RejectsPathTraversal(t *testing.T) {
	// Build a minimal bundle with a tar entry using ../escape.
	// We skip the real ExportBundle path and write the tar
	// manually.
	var raw bytes.Buffer
	tg := craftMaliciousBundle(t, "../escape.txt", []byte("evil"))
	raw.Write(tg)

	if _, err := audit.VerifyBundle(&raw); err == nil {
		t.Error("path-traversal entry should be rejected")
	} else if !strings.Contains(err.Error(), "suspicious") {
		t.Errorf("expected suspicious-path error; got %v", err)
	}
}

// craftMaliciousBundle is a tiny tar.gz builder for the
// path-traversal rejection test.
func craftMaliciousBundle(t *testing.T, name string, body []byte) []byte {
	// We can't easily import archive/tar here without
	// duplicating the bundle.go logic — instead use the helper
	// by exporting a real bundle and tampering with the name
	// pre-tar.  For this test, build a tar.gz inline via the
	// stdlib.
	t.Helper()
	var buf bytes.Buffer
	gzw := newGzipWriter(&buf)
	tw := newTarWriter(gzw)
	hdr := newTarHeader(name, int64(len(body)))
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gzw.Close()
	return buf.Bytes()
}
