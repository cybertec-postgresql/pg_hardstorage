package walsink_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

func encryptedCAS(t *testing.T, sp storage.StoragePlugin, dek [encryption.KeyLen]byte) *repo.CAS {
	t.Helper()
	enc, err := aesgcm.New(dek[:])
	if err != nil {
		t.Fatalf("aesgcm.New: %v", err)
	}
	return casdefault.NewEncrypted(sp, enc)
}

// TestPushSegmentFile_EncryptedRoundTrip is the issue-#106 write-side proof: a
// WAL segment pushed through an encrypting CAS is restorable byte-for-byte by a
// CAS holding the SAME DEK, the manifest records the envelope, and a CAS with a
// DIFFERENT DEK cannot read the chunks (they are genuinely ciphertext at rest).
func TestPushSegmentFile_EncryptedRoundTrip(t *testing.T) {
	sp, _ := openFSRepo(t, "file://"+t.TempDir())
	defer sp.Close()
	ctx := context.Background()

	dek, err := encryption.GenerateDEK()
	if err != nil {
		t.Fatal(err)
	}
	env := &walsink.EncryptionInfo{
		Scheme: "aes-256-gcm", KEKRef: "local:default",
		WrappedDEK: "ZHVtbXktd3JhcHBlZA==", EnvelopeVersion: 2,
	}

	segmentName := "000000010000000000000005"
	segPath := filepath.Join(t.TempDir(), segmentName)
	body := make([]byte, walsink.SegmentSize)
	for i := range body {
		body[i] = byte((i*7 + 3) % 256)
	}
	if err := os.WriteFile(segPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := walsink.PushSegmentFile(ctx, encryptedCAS(t, sp, dek), sp, segPath, walsink.PushOptions{
		Deployment:       "db1",
		SystemIdentifier: "7000000000000000001",
		Encryption:       env,
	})
	if err != nil {
		t.Fatalf("encrypted push: %v", err)
	}
	if m.Encryption == nil || m.Encryption.KEKRef != "local:default" || m.Encryption.Scheme != "aes-256-gcm" {
		t.Fatalf("manifest must record the envelope; got %+v", m.Encryption)
	}

	// Reassemble through a CAS holding the same DEK → must equal the original.
	dec := encryptedCAS(t, sp, dek)
	var got []byte
	for _, c := range m.Chunks {
		bs, err := dec.GetChunkBytes(ctx, c.Hash)
		if err != nil {
			t.Fatalf("decrypt chunk %s: %v", c.Hash, err)
		}
		got = append(got, bs...)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("decrypted segment differs from original (%d vs %d bytes)", len(got), len(body))
	}

	// A different DEK must NOT decrypt — proves the bytes at rest are ciphertext.
	other, _ := encryption.GenerateDEK()
	if _, err := encryptedCAS(t, sp, other).GetChunkBytes(ctx, m.Chunks[0].Hash); err == nil {
		t.Error("a CAS with the wrong DEK must fail to read encrypted WAL chunks")
	}
}

// TestSegmentManifest_PlaintextByteIdentical pins backward compatibility: a
// plaintext segment (Encryption nil) must marshal with NO "encryption" field,
// so existing v1 manifests and idempotent re-pushes stay byte-for-byte
// identical (the omitempty contract + ChunkRefsEqual idempotency).
func TestSegmentManifest_PlaintextByteIdentical(t *testing.T) {
	m := &walsink.SegmentManifest{
		Schema: walsink.Schema, Deployment: "db1", SystemIdentifier: "700",
		Timeline: 1, SegmentNumber: 5, SegmentName: "000000010000000000000005",
		StartLSN: "0/5000000", EndLSN: "0/6000000", SegmentSize: walsink.SegmentSize,
		CreatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	b, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("encryption")) {
		t.Errorf("plaintext manifest must not emit an encryption field:\n%s", b)
	}
	parsed, err := walsink.ParseSegmentManifest(b)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Encryption != nil {
		t.Errorf("plaintext manifest parsed with non-nil Encryption: %+v", parsed.Encryption)
	}
}

// TestSegmentManifest_EncryptionRoundTrip: the envelope survives marshal/parse
// so restore can resolve the DEK from the segment manifest alone.
func TestSegmentManifest_EncryptionRoundTrip(t *testing.T) {
	m := &walsink.SegmentManifest{
		Schema: walsink.Schema, Deployment: "db1", SystemIdentifier: "700",
		Timeline: 1, SegmentNumber: 5, SegmentName: "000000010000000000000005",
		SegmentSize: walsink.SegmentSize, CreatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Encryption: &walsink.EncryptionInfo{
			Scheme: "aes-256-gcm", KEKRef: "local:default",
			WrappedDEK: "d3JhcHBlZA==", EnvelopeVersion: 2,
		},
	}
	b, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := walsink.ParseSegmentManifest(b)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Encryption == nil || parsed.Encryption.KEKRef != "local:default" ||
		parsed.Encryption.WrappedDEK != "d3JhcHBlZA==" || parsed.Encryption.EnvelopeVersion != 2 {
		t.Errorf("envelope did not round-trip: %+v", parsed.Encryption)
	}
}
