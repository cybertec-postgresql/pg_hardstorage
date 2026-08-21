package audit_test

// bundle_signer_identity_test.go — the audit-bundle equivalent of
// "who signed this?".
//
// A bundle ships three things that matter to an auditor: the event
// bodies, a detached signature, and the public key that validates it.
// All three live INSIDE the tarball, so the signature alone only ever
// proves "signed by whoever is in this file" — an attacker who rewrites
// the events can re-sign them under their own key, drop their own
// public_key.pem in, and the maths still checks out.
//
// The thing that makes the identity claim checkable is bundle.json's
// public_key_fingerprint: the auditor compares it against the operator
// fingerprint they know out-of-band. VerifyBundle must therefore refuse
// a bundle whose bundled key does not hash to the fingerprint the
// manifest records — otherwise the attacker simply leaves the victim's
// fingerprint in place and the printed identity is a lie the verifier
// endorsed.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// TestVerifyBundle_ResignedUnderForeignKeyRefused re-signs a bundle
// under an attacker keypair while leaving the honest operator's
// fingerprint in bundle.json. The signature is valid; the identity is
// forged. VerifyBundle must refuse.
func TestVerifyBundle_ResignedUnderForeignKeyRefused(t *testing.T) {
	w := setupBundleWorld(t)
	base := time.Date(2026, 4, 29, 10, 0, 0, 0, time.UTC)
	w.appendEvent(t, "backup.create", "db1", base)
	w.appendEvent(t, "backup.create", "db1", base.Add(time.Hour))

	var honest bytes.Buffer
	if _, err := audit.ExportBundle(context.Background(), w.sp, &honest, w.signer,
		audit.ExportOptions{Operator: "hs", SourceURL: w.repoURL}); err != nil {
		t.Fatal(err)
	}
	// Sanity: the untouched bundle verifies.
	honestCopy := honest.Bytes()
	manifest, err := audit.VerifyBundle(bytes.NewReader(honestCopy))
	if err != nil {
		t.Fatalf("honest bundle should verify: %v", err)
	}
	if manifest.PublicKeyFingerprint == "" {
		t.Fatal("bundle recorded no public_key_fingerprint; the identity binding has nothing to check")
	}

	files, order := readBundleFiles(t, honestCopy)

	// The attacker rewrites history — one event body loses its actor —
	// then re-signs everything under a key they control.
	files["events.ndjson"] = bytes.ReplaceAll(files["events.ndjson"],
		[]byte(`"action":"backup.create"`), []byte(`"action":"backup.delete"`))

	attackerPriv, _, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	attacker, err := backup.LoadSigner(attackerPriv)
	if err != nil {
		t.Fatal(err)
	}
	attackerPEM, err := attacker.PublicKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	files["public_key.pem"] = attackerPEM
	// bundle.json is deliberately left untouched: it still names the
	// honest operator's fingerprint. That is the forged claim.
	files["signature.sig"] = attacker.Sign(canonicalSigningInput(t, files, manifest.SignedFiles))

	forged := writeBundleFiles(t, files, order)

	// The forged bundle's signature is internally valid — prove it, so
	// a failure below can only come from the identity check.
	if !ed25519.Verify(attacker.PublicKey(),
		canonicalSigningInput(t, files, manifest.SignedFiles), files["signature.sig"]) {
		t.Fatal("test bug: re-signed bundle does not verify under the attacker key")
	}

	got, err := audit.VerifyBundle(bytes.NewReader(forged))
	if err == nil {
		t.Fatalf("re-signed bundle verified; manifest claims signer %s but the bundled key is the attacker's",
			got.PublicKeyFingerprint)
	}
	if !strings.Contains(err.Error(), "does not match the manifest's recorded signer") {
		t.Errorf("want signer-fingerprint mismatch error; got %v", err)
	}
}

// TestVerifyBundle_HonestBundleFingerprintMatchesKey pins the positive
// half: the fingerprint an honest bundle records is the fingerprint of
// the key it ships, in the format the rest of the binary prints
// (SHA-256, first 16 hex chars).
func TestVerifyBundle_HonestBundleFingerprintMatchesKey(t *testing.T) {
	w := setupBundleWorld(t)
	w.appendEvent(t, "kms.rotate", "db1", time.Date(2026, 4, 29, 10, 0, 0, 0, time.UTC))

	var raw bytes.Buffer
	if _, err := audit.ExportBundle(context.Background(), w.sp, &raw, w.signer,
		audit.ExportOptions{Operator: "hs", SourceURL: w.repoURL}); err != nil {
		t.Fatal(err)
	}
	manifest, err := audit.VerifyBundle(bytes.NewReader(raw.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(w.signer.PublicKey())
	if want := hex.EncodeToString(sum[:8]); manifest.PublicKeyFingerprint != want {
		t.Errorf("public_key_fingerprint = %q; want %q", manifest.PublicKeyFingerprint, want)
	}
}

// readBundleFiles unpacks a bundle tarball into name→body plus the
// original entry order (rebuilding in a different order would be a
// second, unrelated tamper).
func readBundleFiles(t *testing.T, raw []byte) (map[string][]byte, []string) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	var order []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[hdr.Name] = body
		order = append(order, hdr.Name)
	}
	return files, order
}

// writeBundleFiles is readBundleFiles' inverse.
func writeBundleFiles(t *testing.T, files map[string][]byte, order []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := newGzipWriter(&buf)
	tw := newTarWriter(gzw)
	for _, name := range order {
		body := files[name]
		if err := tw.WriteHeader(newTarHeader(name, int64(len(body)))); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// canonicalSigningInput reproduces bundle.go's canonicalBundleBytes
// from the auditor's side of the fence — the documented format any
// third-party verifier implements:
//
//	<file count> u64be, then per file: <name len> u32be, name,
//	<body len> u64be, body — in manifest.signed_files order.
func canonicalSigningInput(t *testing.T, files map[string][]byte, signedFiles []string) []byte {
	t.Helper()
	var out []byte
	out = appendBE(out, uint64(len(signedFiles)), 8)
	for _, name := range signedFiles {
		body, ok := files[name]
		if !ok {
			t.Fatalf("signed file %q missing from bundle", name)
		}
		out = appendBE(out, uint64(len(name)), 4)
		out = append(out, name...)
		out = appendBE(out, uint64(len(body)), 8)
		out = append(out, body...)
	}
	return out
}

func appendBE(b []byte, v uint64, width int) []byte {
	for i := width - 1; i >= 0; i-- {
		b = append(b, byte(v>>(8*uint(i))))
	}
	return b
}
