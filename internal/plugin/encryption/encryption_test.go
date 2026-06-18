package encryption_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, encryption.KeyLen)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestAESGCM_RoundTrip(t *testing.T) {
	enc, err := aesgcm.New(newKey(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, sz := range []int{0, 1, 16, 1024, 64 * 1024} {
		plaintext := make([]byte, sz)
		_, _ = rand.Read(plaintext)
		ct, nonce, err := enc.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("size %d: Encrypt: %v", sz, err)
		}
		// Ciphertext is plaintext + 16-byte AEAD tag.
		if len(ct) != sz+16 {
			t.Errorf("size %d: ct len = %d, want %d (plaintext + 16 tag)", sz, len(ct), sz+16)
		}
		// Nonce should be non-zero (with overwhelming probability for
		// any non-empty key).
		var zero [encryption.NonceLen]byte
		if sz > 0 && nonce == zero {
			t.Errorf("nonce is zero — random nonce drawing failed?")
		}
		got, err := enc.Decrypt(ct, nonce)
		if err != nil {
			t.Fatalf("size %d: Decrypt: %v", sz, err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Errorf("size %d: round-trip differs", sz)
		}
	}
}

func TestAESGCM_NoncesAreDistinct(t *testing.T) {
	// Two encrypts of the same plaintext under the same key MUST
	// produce different nonces — the random-nonce property we rely
	// on for AEAD safety. Two encrypts SHOULD therefore produce
	// different ciphertexts too.
	enc, _ := aesgcm.New(newKey(t))
	pt := bytes.Repeat([]byte("repeat"), 64)
	ct1, n1, _ := enc.Encrypt(pt)
	ct2, n2, _ := enc.Encrypt(pt)
	if n1 == n2 {
		t.Error("two consecutive Encrypts produced the same nonce")
	}
	if bytes.Equal(ct1, ct2) {
		t.Error("two consecutive Encrypts produced the same ciphertext")
	}
}

func TestAESGCM_TamperedCiphertextFails(t *testing.T) {
	enc, _ := aesgcm.New(newKey(t))
	pt := []byte("we will tamper with this")
	ct, nonce, _ := enc.Encrypt(pt)
	// Flip a bit in the ciphertext.
	ct[0] ^= 0x01
	_, err := enc.Decrypt(ct, nonce)
	if !errors.Is(err, encryption.ErrAuthenticationFailed) {
		t.Errorf("tampered ciphertext should yield ErrAuthenticationFailed; got %v", err)
	}
}

func TestAESGCM_TamperedTagFails(t *testing.T) {
	enc, _ := aesgcm.New(newKey(t))
	pt := []byte("the tag is the last 16 bytes")
	ct, nonce, _ := enc.Encrypt(pt)
	// Flip a bit in the tag.
	ct[len(ct)-1] ^= 0x01
	_, err := enc.Decrypt(ct, nonce)
	if !errors.Is(err, encryption.ErrAuthenticationFailed) {
		t.Errorf("tampered tag should yield ErrAuthenticationFailed; got %v", err)
	}
}

func TestAESGCM_WrongNonceFails(t *testing.T) {
	enc, _ := aesgcm.New(newKey(t))
	pt := []byte("plaintext")
	ct, _, _ := enc.Encrypt(pt)
	var wrong [encryption.NonceLen]byte
	wrong[0] = 0xFF
	_, err := enc.Decrypt(ct, wrong)
	if !errors.Is(err, encryption.ErrAuthenticationFailed) {
		t.Errorf("wrong nonce should yield ErrAuthenticationFailed; got %v", err)
	}
}

func TestAESGCM_WrongKeyFails(t *testing.T) {
	encA, _ := aesgcm.New(newKey(t))
	encB, _ := aesgcm.New(newKey(t))
	ct, nonce, _ := encA.Encrypt([]byte("only A can read this"))
	_, err := encB.Decrypt(ct, nonce)
	if !errors.Is(err, encryption.ErrAuthenticationFailed) {
		t.Errorf("decrypt with foreign key should fail; got %v", err)
	}
}

func TestAESGCM_RejectsBadKeySize(t *testing.T) {
	cases := []int{0, 1, 16, 24, 31, 33, 64}
	for _, sz := range cases {
		k := make([]byte, sz)
		_, err := aesgcm.New(k)
		if err == nil {
			t.Errorf("aesgcm.New accepted %d-byte key; want %d-byte only", sz, encryption.KeyLen)
			continue
		}
		if !errors.Is(err, encryption.ErrInvalidKey) {
			t.Errorf("size %d: error %v should wrap ErrInvalidKey", sz, err)
		}
	}
}

func TestAESGCM_NameAndAlgorithm(t *testing.T) {
	enc, _ := aesgcm.New(newKey(t))
	if enc.Name() != "aes-256-gcm" {
		t.Errorf("Name = %q", enc.Name())
	}
	if enc.Algorithm() != encryption.AlgoAESGCM {
		t.Errorf("Algorithm = %d", enc.Algorithm())
	}
}

func TestRegistry_LookupAndHas(t *testing.T) {
	enc, _ := aesgcm.New(newKey(t))
	r := encryption.NewRegistry()
	r.Register(encryption.AlgoAESGCM, enc)
	if !r.Has(encryption.AlgoAESGCM) {
		t.Error("Has should report registered algos")
	}
	got, err := r.Lookup(encryption.AlgoAESGCM)
	if err != nil || got == nil {
		t.Errorf("Lookup err=%v got=%v", err, got)
	}
	_, err = r.Lookup(encryption.AlgorithmID(99))
	if !errors.Is(err, encryption.ErrUnknownAlgorithm) {
		t.Errorf("unknown algo lookup should return ErrUnknownAlgorithm; got %v", err)
	}
}

func TestRegistry_DoubleRegisterPanics(t *testing.T) {
	enc, _ := aesgcm.New(newKey(t))
	r := encryption.NewRegistry()
	r.Register(encryption.AlgoAESGCM, enc)
	defer func() {
		if rec := recover(); rec == nil {
			t.Error("expected panic on double-register")
		}
	}()
	r.Register(encryption.AlgoAESGCM, enc)
}

func TestAlgorithmID_String(t *testing.T) {
	cases := []struct {
		a    encryption.AlgorithmID
		want string
	}{
		{encryption.AlgoNone, "none"},
		{encryption.AlgoAESGCM, "aes-256-gcm"},
		{encryption.AlgorithmID(99), "unknown-encryption-algo-99"},
	}
	for _, c := range cases {
		if got := c.a.String(); got != c.want {
			t.Errorf("%d.String() = %q, want %q", c.a, got, c.want)
		}
	}
}

func TestEncryptionFields_IsEncrypted_NotInThisPackage(t *testing.T) {
	// EncryptionFields lives in the compression package (envelope
	// shape is shared); this is just a sanity check that the
	// constants line up — encryption.AlgoNone == 0 must match the
	// "not encrypted" zero value the envelope reader expects.
	if encryption.AlgoNone != 0 {
		t.Errorf("AlgoNone = %d, want 0", encryption.AlgoNone)
	}
	// And likewise that AlgoAESGCM has the expected on-disk byte.
	if encryption.AlgoAESGCM != 1 {
		t.Errorf("AlgoAESGCM = %d, want 1 (the envelope byte)", encryption.AlgoAESGCM)
	}
	_ = strings.Contains
}
