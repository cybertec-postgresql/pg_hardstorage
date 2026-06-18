package backup_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

func TestGenerateKeypair_PEMShape(t *testing.T) {
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(priv, []byte("BEGIN PG_HARDSTORAGE ED25519 PRIVATE KEY")) {
		t.Errorf("private PEM missing header: %s", priv)
	}
	if !bytes.Contains(pub, []byte("BEGIN PG_HARDSTORAGE ED25519 PUBLIC KEY")) {
		t.Errorf("public PEM missing header: %s", pub)
	}
}

func TestSignVerify_RoundTrip(t *testing.T) {
	privPEM, pubPEM, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := backup.LoadSigner(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := backup.LoadVerifier(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("the canonical bytes of a manifest")
	sig := signer.Sign(payload)
	if len(sig) != 64 {
		t.Errorf("Ed25519 signature should be 64 bytes; got %d", len(sig))
	}
	if err := verifier.Verify(payload, sig); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestVerify_RejectsTamperedPayload(t *testing.T) {
	privPEM, pubPEM, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(privPEM)
	verifier, _ := backup.LoadVerifier(pubPEM)

	payload := []byte("original")
	sig := signer.Sign(payload)
	tampered := []byte("Original")
	err := verifier.Verify(tampered, sig)
	if err == nil {
		t.Fatal("verify must reject tampered payload")
	}
	if !errors.Is(err, backup.ErrBadSignature) {
		t.Errorf("expected ErrBadSignature; got %v", err)
	}
}

func TestVerify_RejectsTamperedSignature(t *testing.T) {
	privPEM, pubPEM, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(privPEM)
	verifier, _ := backup.LoadVerifier(pubPEM)

	payload := []byte("hello")
	sig := signer.Sign(payload)
	sig[0] ^= 0x01 // flip one bit
	if err := verifier.Verify(payload, sig); !errors.Is(err, backup.ErrBadSignature) {
		t.Errorf("expected ErrBadSignature; got %v", err)
	}
}

func TestVerify_RejectsCrossKey(t *testing.T) {
	priv1, _, _ := backup.GenerateKeypair(rand.Reader)
	_, pub2, _ := backup.GenerateKeypair(rand.Reader)
	signer1, _ := backup.LoadSigner(priv1)
	verifier2, _ := backup.LoadVerifier(pub2)

	payload := []byte("hello")
	sig := signer1.Sign(payload)
	if err := verifier2.Verify(payload, sig); !errors.Is(err, backup.ErrBadSignature) {
		t.Errorf("verifier from a different keypair must reject; got %v", err)
	}
}

func TestSigner_PublicKeyPEM_MatchesGenerated(t *testing.T) {
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	got, err := signer.PublicKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pub) {
		t.Errorf("PublicKeyPEM differs from the GenerateKeypair output")
	}
}

func TestLoadSigner_RejectsGarbage(t *testing.T) {
	for _, in := range [][]byte{
		nil,
		[]byte(""),
		[]byte("not PEM at all"),
		[]byte("-----BEGIN UNRELATED-----\nAAAA\n-----END UNRELATED-----\n"),
	} {
		if _, err := backup.LoadSigner(in); err == nil {
			t.Errorf("LoadSigner(%q) should error", in)
		}
	}
}

func TestLoadVerifier_RejectsGarbage(t *testing.T) {
	for _, in := range [][]byte{
		nil,
		[]byte(""),
		[]byte("not PEM at all"),
	} {
		if _, err := backup.LoadVerifier(in); err == nil {
			t.Errorf("LoadVerifier(%q) should error", in)
		}
	}
}

func TestLoadSigner_RejectsPublicKeyAsPrivate(t *testing.T) {
	_, pub, _ := backup.GenerateKeypair(rand.Reader)
	if _, err := backup.LoadSigner(pub); err == nil {
		t.Error("LoadSigner must refuse a public-key PEM block")
	}
}
