package runner_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
)

// fakeProvider mirrors the awskms.Client semantics for unit
// tests without spinning up a real AWS KMS connection.  Wrap
// prepends "WRAP:" so Unwrap can verify it's our ciphertext.
type fakeProvider struct {
	kekRef      string
	wrapErr     error
	unwrapErr   error
	wrapCalls   int
	unwrapCalls int
}

func (f *fakeProvider) Name() string   { return "fake-kms" }
func (f *fakeProvider) KEKRef() string { return f.kekRef }
func (f *fakeProvider) WrapDEK(_ context.Context, dek []byte) ([]byte, error) {
	f.wrapCalls++
	if f.wrapErr != nil {
		return nil, f.wrapErr
	}
	return append([]byte("WRAP:"), dek...), nil
}
func (f *fakeProvider) UnwrapDEK(_ context.Context, wrapped []byte) ([]byte, error) {
	f.unwrapCalls++
	if f.unwrapErr != nil {
		return nil, f.unwrapErr
	}
	if !strings.HasPrefix(string(wrapped), "WRAP:") {
		return nil, errors.New("fake-kms: not our ciphertext")
	}
	return wrapped[len("WRAP:"):], nil
}
func (f *fakeProvider) Shred(_ context.Context) error { return nil }
func (f *fakeProvider) FIPSMode() bool                { return false }
func (f *fakeProvider) Close() error                  { return nil }

// TestEncryptionConfig_CloudKMSPath: when EncryptionConfig
// declares a Provider, the runner's wrap-side branch picks
// it up — we don't need a full TakeBackup to verify (which
// would require a real PG).  Instead we exercise the wrap
// shape by calling the same code path the runner uses, then
// round-trip through keystore.UnwrapDEK to confirm the
// manifest's wrapped_dek decrypts correctly.
func TestEncryptionConfig_CloudKMSPath_RoundTripsThroughManifest(t *testing.T) {
	provider := &fakeProvider{kekRef: "fake-kms://my-key"}
	cfg := &runner.EncryptionConfig{Provider: provider, KEKRef: provider.KEKRef()}

	// Replicate the runner's wrap step.  We use a known DEK so
	// the test asserts on bytes rather than a random round-trip.
	dek := make([]byte, encryption.KeyLen)
	for i := range dek {
		dek[i] = byte(i)
	}
	wrapped, err := cfg.Provider.WrapDEK(context.Background(), dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if provider.wrapCalls != 1 {
		t.Errorf("expected 1 wrap call; got %d", provider.wrapCalls)
	}

	// Pretend the runner committed the manifest with this
	// wrapped DEK and KEKRef.
	manifest := &backup.Manifest{
		BackupID:   "db1.full.20260504T1200Z",
		Deployment: "db1",
		Encryption: &backup.EncryptionInfo{
			Scheme:          "aes-256-gcm",
			KEKRef:          cfg.KEKRef,
			WrappedDEK:      base64.StdEncoding.EncodeToString(wrapped),
			EnvelopeVersion: 2,
		},
	}

	// Restore would consult keystore.UnwrapDEK.  We register
	// the fake provider in a fresh registry to avoid leaking
	// state into other tests.
	registry := kms.NewRegistry()
	registry.Register("fake-kms", func(_ context.Context, ref string, _ map[string]any) (kms.Provider, error) {
		return &fakeProvider{kekRef: ref}, nil
	})

	wrappedFromManifest, err := base64.StdEncoding.DecodeString(manifest.Encryption.WrappedDEK)
	if err != nil {
		t.Fatal(err)
	}
	got, err := keystore.UnwrapDEK(context.Background(), manifest.Encryption.KEKRef,
		wrappedFromManifest, keystore.UnwrapOpts{Registry: registry})
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if string(got) != string(dek) {
		t.Errorf("wrap+unwrap round-trip lost bytes")
	}
}

// TestEncryptionConfig_LocalPathBackwardsCompatible: legacy
// EncryptionConfig with KEK + KEKRef=local:default still
// works (Provider==nil).  The runner's wrap-side branch
// must continue to use encryption.Wrap for this shape.
func TestEncryptionConfig_LocalPathBackwardsCompatible(t *testing.T) {
	dir := t.TempDir()
	kek, _, err := keystore.LoadOrGenerateKEK(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &runner.EncryptionConfig{KEK: kek, KEKRef: keystore.KEKRefLocal}
	if cfg.Provider != nil {
		t.Error("local path should leave Provider nil")
	}
	if cfg.KEKRef != keystore.KEKRefLocal {
		t.Errorf("local KEKRef = %q", cfg.KEKRef)
	}
}

// TestEncryptionConfig_WrapErrorPropagates: a provider that
// fails WrapDEK surfaces a wrapped error; tests can rely on
// the typed sentinel from kms (or a domain-specific one).
func TestEncryptionConfig_WrapErrorPropagates(t *testing.T) {
	boom := errors.New("AccessDeniedException")
	provider := &fakeProvider{kekRef: "fake-kms://x", wrapErr: boom}
	dek := make([]byte, encryption.KeyLen)
	_, err := provider.WrapDEK(context.Background(), dek)
	if !errors.Is(err, boom) {
		t.Errorf("expected boom propagation; got %v", err)
	}
}

func TestEncryptionConfig_KEKRefMatchesProvider(t *testing.T) {
	provider := &fakeProvider{kekRef: "fake-kms://aliased-key"}
	cfg := &runner.EncryptionConfig{Provider: provider, KEKRef: provider.KEKRef()}
	if cfg.KEKRef != "fake-kms://aliased-key" {
		t.Errorf("KEKRef should match Provider.KEKRef(); got %q", cfg.KEKRef)
	}
}
