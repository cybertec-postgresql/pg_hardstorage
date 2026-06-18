package backup_test

import (
	"bytes"
	"crypto/rand"
	stdjson "encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// sampleManifest returns a small, fully-populated Manifest useful for
// round-trip and signing tests.
//
// Every field Manifest.Validate gates on is populated so the manifest
// can be Commit()'d without tripping the issue #91 validation gate.
func sampleManifest() *backup.Manifest {
	return &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "db1.full.20260428T1200Z",
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        170,
		SystemIdentifier: "7388123456789012345",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC),
		StoppedAt:        time.Date(2026, 4, 28, 12, 8, 23, 0, time.UTC),
		Compression:      "none",
		Tablespaces: []backup.Tablespace{
			{OID: 1663, Location: "pg_default"},
		},
		Files: []backup.FileEntry{
			{
				Path: "base/16384/2619",
				Size: 8192,
				Mode: 0o644,
				Chunks: []backup.ChunkRef{
					{Hash: repo.HashOf([]byte("alpha")), Offset: 0, Len: 4096},
					{Hash: repo.HashOf([]byte("beta")), Offset: 4096, Len: 4096},
				},
			},
		},
		BackupLabel: "START WAL LOCATION: 0/3000028 (file 000000010000000000000003)\n" +
			"CHECKPOINT LOCATION: 0/3000028\n" +
			"BACKUP METHOD: streamed\n" +
			"BACKUP FROM: primary\n" +
			"START TIME: 2026-04-28 12:00:00 UTC\n" +
			"LABEL: pg_hardstorage_db1\n",
		WALRequired: []string{"000000010000000000000003"},
	}
}

func TestCanonicalize_Deterministic(t *testing.T) {
	m := sampleManifest()
	a, err := m.Canonicalize()
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Canonicalize()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("canonical bytes not deterministic across two runs")
	}
}

func TestCanonicalize_OmitsAttestation(t *testing.T) {
	m := sampleManifest()
	m.Attestation = &backup.Attestation{Scheme: backup.SchemeEd25519, Signature: "xxx"}
	canonical, err := m.Canonicalize()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(canonical, []byte("attestation")) {
		t.Errorf("Canonicalize must zero the attestation field; got %s", canonical)
	}
}

func TestCanonicalize_DoesNotMutateOriginal(t *testing.T) {
	m := sampleManifest()
	m.Attestation = &backup.Attestation{Scheme: backup.SchemeEd25519, Signature: "xxx"}
	if _, err := m.Canonicalize(); err != nil {
		t.Fatal(err)
	}
	if m.Attestation == nil {
		t.Error("Canonicalize must not zero the receiver's Attestation")
	}
}

func TestCanonicalize_NoTrailingNewline(t *testing.T) {
	m := sampleManifest()
	c, err := m.Canonicalize()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(c); n > 0 && c[n-1] == '\n' {
		t.Error("canonical bytes must not have a trailing newline (would break round-trip with json.Encoder)")
	}
}

func TestCanonicalize_StableAcrossRoundTrip(t *testing.T) {
	// Marshal -> unmarshal -> canonicalize must equal the original
	// canonicalize. This is the property signature verification relies
	// on: a parsed-from-disk manifest reconstructs the same bytes the
	// signer signed.
	m := sampleManifest()
	first, err := m.Canonicalize()
	if err != nil {
		t.Fatal(err)
	}

	var roundTripped backup.Manifest
	if err := stdjson.Unmarshal(first, &roundTripped); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	second, err := roundTripped.Canonicalize()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("canonical bytes differ across round-trip\nfirst:  %s\nsecond: %s", first, second)
	}
}

func TestSignAndVerify_RoundTrip(t *testing.T) {
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	m := sampleManifest()
	if err := m.Sign(signer); err != nil {
		t.Fatal(err)
	}
	if m.Attestation == nil {
		t.Fatal("Sign must populate Attestation")
	}
	if m.Attestation.Scheme != backup.SchemeEd25519 {
		t.Errorf("Attestation.Scheme = %q", m.Attestation.Scheme)
	}

	raw, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}

	got, err := backup.ParseAndVerify(raw, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if got.BackupID != m.BackupID {
		t.Errorf("BackupID round-trip: got %q, want %q", got.BackupID, m.BackupID)
	}
}

func TestParseAndVerify_RejectsTamperedField(t *testing.T) {
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	m := sampleManifest()
	if err := m.Sign(signer); err != nil {
		t.Fatal(err)
	}
	raw, _ := m.MarshalToBytes()

	// Tamper: change BackupID inside the JSON. Replace one character so
	// the JSON stays well-formed.
	tampered := bytes.Replace(raw, []byte("db1.full.20260428T1200Z"), []byte("db1.full.20260428T1300Z"), 1)
	if bytes.Equal(raw, tampered) {
		t.Fatal("test setup: tampering replacement did not change the bytes")
	}
	_, err := backup.ParseAndVerify(tampered, verifier)
	if !errors.Is(err, backup.ErrBadSignature) {
		t.Errorf("expected ErrBadSignature; got %v", err)
	}
}

func TestParseAndVerify_RejectsTamperedSignature(t *testing.T) {
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	m := sampleManifest()
	_ = m.Sign(signer)
	// Replace the signature with all-zeros (still base64-decodable).
	m.Attestation.Signature = strings.Repeat("AA", 64) // base64 zeros
	raw, _ := m.MarshalToBytes()

	_, err := backup.ParseAndVerify(raw, verifier)
	if !errors.Is(err, backup.ErrBadSignature) {
		t.Errorf("expected ErrBadSignature; got %v", err)
	}
}

func TestParseAndVerify_RejectsForeignKey(t *testing.T) {
	// Manifest signed by key A; verifier holds key B. Even though the
	// signature is internally valid, ParseAndVerify must refuse because
	// the public-key fingerprint mismatches.
	priv1, _, _ := backup.GenerateKeypair(rand.Reader)
	_, pub2, _ := backup.GenerateKeypair(rand.Reader)
	signer1, _ := backup.LoadSigner(priv1)
	verifier2, _ := backup.LoadVerifier(pub2)

	m := sampleManifest()
	_ = m.Sign(signer1)
	raw, _ := m.MarshalToBytes()

	_, err := backup.ParseAndVerify(raw, verifier2)
	if !errors.Is(err, backup.ErrPublicKeyMismatch) {
		t.Errorf("expected ErrPublicKeyMismatch; got %v", err)
	}
}

func TestParseAndVerify_RejectsUnsigned(t *testing.T) {
	priv, _, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	pubPEM, _ := signer.PublicKeyPEM()
	verifier, _ := backup.LoadVerifier(pubPEM)

	m := sampleManifest()
	// Skip Sign; emit raw bytes without an Attestation block.
	raw, _ := m.MarshalToBytes()
	if _, err := backup.ParseAndVerify(raw, verifier); !errors.Is(err, backup.ErrUnsigned) {
		t.Errorf("expected ErrUnsigned; got %v", err)
	}
}

func TestParseAndVerify_RejectsBadSchema(t *testing.T) {
	priv, _, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	pubPEM, _ := signer.PublicKeyPEM()
	verifier, _ := backup.LoadVerifier(pubPEM)

	m := sampleManifest()
	m.Schema = "pg_hardstorage.manifest.v999"
	_ = m.Sign(signer)
	raw, _ := m.MarshalToBytes()

	if _, err := backup.ParseAndVerify(raw, verifier); err == nil {
		t.Error("foreign schema should fail ParseAndVerify")
	}
}

func TestMarshalToBytes_HexHashes(t *testing.T) {
	// Confirm that ChunkRef.Hash renders as 64-char hex (not as a
	// JSON array of integers, which would happen with bare [32]byte).
	m := sampleManifest()
	raw, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(m.Files[0].Chunks[0].Hash.String())) {
		t.Errorf("manifest JSON did not contain hex hash:\n%s", raw)
	}
	if bytes.Contains(raw, []byte("[171,")) {
		t.Errorf("manifest JSON contains a number-array hash (Hash type's MarshalText not used):\n%s", raw)
	}
}
