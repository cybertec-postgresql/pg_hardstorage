package encryption_test

import (
	"crypto/rand"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
)

func newKEK(t *testing.T) [encryption.KeyLen]byte {
	t.Helper()
	var kek [encryption.KeyLen]byte
	if _, err := rand.Read(kek[:]); err != nil {
		t.Fatal(err)
	}
	return kek
}

func TestWrap_RoundTrip(t *testing.T) {
	kek := newKEK(t)
	dek, err := encryption.GenerateDEK()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := encryption.Wrap(kek, dek)
	if err != nil {
		t.Fatal(err)
	}
	if len(wrapped) != encryption.WrappedKeyLen {
		t.Errorf("wrapped len = %d, want %d", len(wrapped), encryption.WrappedKeyLen)
	}
	got, err := encryption.Unwrap(kek, wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if got != dek {
		t.Error("DEK round-trip differs")
	}
}

func TestWrap_TwoCallsProduceDifferentBlobs(t *testing.T) {
	// Random nonce in each Wrap → distinct outputs even for the
	// same (kek, dek) pair.
	kek := newKEK(t)
	dek, _ := encryption.GenerateDEK()
	w1, err := encryption.Wrap(kek, dek)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := encryption.Wrap(kek, dek)
	if err != nil {
		t.Fatal(err)
	}
	equal := true
	for i := range w1 {
		if w1[i] != w2[i] {
			equal = false
			break
		}
	}
	if equal {
		t.Error("two Wrap calls produced identical bytes — random nonce broken?")
	}
}

func TestUnwrap_ForeignKEK_Fails(t *testing.T) {
	kekA := newKEK(t)
	kekB := newKEK(t)
	dek, _ := encryption.GenerateDEK()
	wrapped, err := encryption.Wrap(kekA, dek)
	if err != nil {
		t.Fatal(err)
	}
	_, err = encryption.Unwrap(kekB, wrapped)
	if !errors.Is(err, encryption.ErrAuthenticationFailed) {
		t.Errorf("unwrap with foreign KEK should fail with ErrAuthenticationFailed; got %v", err)
	}
}

func TestUnwrap_TamperedBytes_Fail(t *testing.T) {
	kek := newKEK(t)
	dek, _ := encryption.GenerateDEK()
	wrapped, err := encryption.Wrap(kek, dek)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a bit in each region in turn (nonce, ciphertext, tag).
	for _, idx := range []int{0, encryption.NonceLen, len(wrapped) - 1} {
		c := append([]byte{}, wrapped...)
		c[idx] ^= 0x01
		_, err := encryption.Unwrap(kek, c)
		if !errors.Is(err, encryption.ErrAuthenticationFailed) {
			t.Errorf("tamper at idx %d should fail; got %v", idx, err)
		}
	}
}

func TestUnwrap_WrongLength_Fails(t *testing.T) {
	kek := newKEK(t)
	for _, sz := range []int{0, 1, encryption.WrappedKeyLen - 1, encryption.WrappedKeyLen + 1} {
		_, err := encryption.Unwrap(kek, make([]byte, sz))
		if !errors.Is(err, encryption.ErrAuthenticationFailed) {
			t.Errorf("size %d should fail with ErrAuthenticationFailed; got %v", sz, err)
		}
	}
}

func TestGenerateDEK_RandomNonZero(t *testing.T) {
	d1, err := encryption.GenerateDEK()
	if err != nil {
		t.Fatal(err)
	}
	d2, err := encryption.GenerateDEK()
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d2 {
		t.Error("two GenerateDEK calls produced identical keys — RNG broken?")
	}
	var zero [encryption.KeyLen]byte
	if d1 == zero {
		t.Error("generated DEK is all-zero")
	}
}
