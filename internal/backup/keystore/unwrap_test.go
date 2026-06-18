package keystore_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
)

// fakeProvider implements kms.Provider for unit tests.  Wrap
// just appends "wrap-" + plaintext; unwrap strips it.
type fakeProvider struct {
	kekRef string
}

func (f *fakeProvider) Name() string   { return "fake" }
func (f *fakeProvider) KEKRef() string { return f.kekRef }
func (f *fakeProvider) WrapDEK(_ context.Context, dek []byte) ([]byte, error) {
	return append([]byte("wrap-"), dek...), nil
}
func (f *fakeProvider) UnwrapDEK(_ context.Context, wrapped []byte) ([]byte, error) {
	if !strings.HasPrefix(string(wrapped), "wrap-") {
		return nil, errors.New("fake: not our ciphertext")
	}
	return wrapped[len("wrap-"):], nil
}
func (f *fakeProvider) Shred(_ context.Context) error { return nil }
func (f *fakeProvider) FIPSMode() bool                { return false }
func (f *fakeProvider) Close() error                  { return nil }

func TestUnwrapDEK_LocalPath(t *testing.T) {
	dir := t.TempDir()
	kek, _, err := keystore.LoadOrGenerateKEK(dir)
	if err != nil {
		t.Fatal(err)
	}
	dek := make([]byte, encryption.KeyLen)
	rand.Read(dek)

	// Wrap the DEK with the KEK using AES-GCM (the on-disk
	// shape every committed v0.1..v0.9 backup uses).
	block, _ := aes.NewCipher(kek[:])
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, 12)
	rand.Read(nonce)
	ct := gcm.Seal(nil, nonce, dek, nil)
	wrapped := append(append([]byte{}, nonce...), ct...)

	// Round-trip through UnwrapDEK with the local KEKRef.
	got, err := keystore.UnwrapDEK(context.Background(), keystore.KEKRefLocal,
		wrapped, keystore.UnwrapOpts{KeyringDir: dir})
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if string(got) != string(dek) {
		t.Errorf("round-trip lost bytes")
	}
}

func TestUnwrapDEK_LocalRequiresKeyringDir(t *testing.T) {
	_, err := keystore.UnwrapDEK(context.Background(), keystore.KEKRefLocal,
		make([]byte, 64), keystore.UnwrapOpts{})
	if err == nil {
		t.Fatal("expected error without KeyringDir")
	}
	if !strings.Contains(err.Error(), "KeyringDir") {
		t.Errorf("error should mention KeyringDir: %v", err)
	}
}

func TestUnwrapDEK_LocalEmptyKEKRefFallsBackToLocal(t *testing.T) {
	// Empty KEKRef is the v0.1 shape (manifests written
	// before the field was populated default to local).
	dir := t.TempDir()
	kek, _, err := keystore.LoadOrGenerateKEK(dir)
	if err != nil {
		t.Fatal(err)
	}
	dek := make([]byte, encryption.KeyLen)
	rand.Read(dek)
	block, _ := aes.NewCipher(kek[:])
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, 12)
	rand.Read(nonce)
	ct := gcm.Seal(nil, nonce, dek, nil)
	wrapped := append(append([]byte{}, nonce...), ct...)

	got, err := keystore.UnwrapDEK(context.Background(), "",
		wrapped, keystore.UnwrapOpts{KeyringDir: dir})
	if err != nil {
		t.Fatalf("UnwrapDEK with empty ref: %v", err)
	}
	if string(got) != string(dek) {
		t.Errorf("empty KEKRef should round-trip via local")
	}
}

func TestUnwrapDEK_LocalAuthFailureWrapsSentinel(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := keystore.LoadOrGenerateKEK(dir); err != nil {
		t.Fatal(err)
	}
	// Forge wrapped bytes that won't decrypt under the
	// just-generated KEK.
	garbage := make([]byte, 12+encryption.KeyLen+16)
	rand.Read(garbage)

	_, err := keystore.UnwrapDEK(context.Background(), keystore.KEKRefLocal,
		garbage, keystore.UnwrapOpts{KeyringDir: dir})
	if err == nil {
		t.Fatal("expected auth failure")
	}
	if !errors.Is(err, encryption.ErrAuthenticationFailed) {
		t.Errorf("expected ErrAuthenticationFailed wrapping; got %v", err)
	}
}

func TestUnwrapDEK_CloudKMS(t *testing.T) {
	registry := kms.NewRegistry()
	registry.Register("fake", func(_ context.Context, ref string, _ map[string]any) (kms.Provider, error) {
		return &fakeProvider{kekRef: ref}, nil
	})

	dek := make([]byte, encryption.KeyLen)
	rand.Read(dek)
	wrapped := append([]byte("wrap-"), dek...)

	got, err := keystore.UnwrapDEK(context.Background(), "fake://my-key",
		wrapped, keystore.UnwrapOpts{Registry: registry})
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if string(got) != string(dek) {
		t.Errorf("round-trip lost bytes")
	}
}

func TestUnwrapDEK_UnknownSchemeRefuses(t *testing.T) {
	_, err := keystore.UnwrapDEK(context.Background(), "nonsense://x",
		[]byte("anything"), keystore.UnwrapOpts{Registry: kms.NewRegistry()})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, kms.ErrUnknownScheme) {
		t.Errorf("expected ErrUnknownScheme; got %v", err)
	}
}

func TestUnwrapDEK_ProviderReturnsWrongLengthRefuses(t *testing.T) {
	registry := kms.NewRegistry()
	registry.Register("bad", func(_ context.Context, _ string, _ map[string]any) (kms.Provider, error) {
		return &shortProvider{}, nil
	})
	_, err := keystore.UnwrapDEK(context.Background(), "bad://x",
		[]byte("wrap-anything"), keystore.UnwrapOpts{Registry: registry})
	if err == nil || !strings.Contains(err.Error(), "want 32") {
		t.Errorf("expected length-mismatch refusal; got %v", err)
	}
}

type shortProvider struct{}

func (shortProvider) Name() string                                        { return "bad" }
func (shortProvider) KEKRef() string                                      { return "bad://x" }
func (shortProvider) WrapDEK(_ context.Context, _ []byte) ([]byte, error) { return nil, nil }
func (shortProvider) UnwrapDEK(_ context.Context, _ []byte) ([]byte, error) {
	return []byte("too-short"), nil
}
func (shortProvider) Shred(_ context.Context) error { return nil }
func (shortProvider) FIPSMode() bool                { return false }
func (shortProvider) Close() error                  { return nil }

func TestSchemeOf_Reexport(t *testing.T) {
	if got := keystore.SchemeOf("aws-kms://x"); got != "aws-kms" {
		t.Errorf("SchemeOf = %q", got)
	}
}
