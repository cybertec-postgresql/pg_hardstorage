package backup_test

import (
	"crypto/rand"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// TestVerifyEmbedded underpins the repair-attestation fix: it must accept a
// manifest that's self-consistent under its OWN embedded key (the
// key-rotation case `repair attestation` exists for) and reject one whose
// content was altered after signing (which re-signing would launder).
func TestVerifyEmbedded(t *testing.T) {
	priv, _, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := backup.LoadSigner(priv)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Self-consistent manifest — signed by some key (which need not be a
	//    trusted verifier; that's the rotation case). Must pass.
	m := sampleManifest()
	if err := m.Sign(signer); err != nil {
		t.Fatal(err)
	}
	raw, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backup.VerifyEmbedded(raw); err != nil {
		t.Errorf("self-consistent (key-rotation) manifest must pass VerifyEmbedded; got %v", err)
	}

	// 2. Tamper the signed content WITHOUT re-signing — must fail.
	tampered := *m // shares the original *Attestation (signature over the original content)
	tampered.Deployment = m.Deployment + "-tampered"
	tamperedRaw, err := tampered.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backup.VerifyEmbedded(tamperedRaw); err == nil {
		t.Error("a manifest whose content was altered after signing must FAIL VerifyEmbedded (re-signing it would launder tampering)")
	}

	// 3. Unsigned manifest → ErrUnsigned.
	u := sampleManifest()
	uRaw, err := u.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backup.VerifyEmbedded(uRaw); !errors.Is(err, backup.ErrUnsigned) {
		t.Errorf("unsigned manifest: want ErrUnsigned, got %v", err)
	}
}
