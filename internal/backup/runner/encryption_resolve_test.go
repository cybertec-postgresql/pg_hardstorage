package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
)

// fakeProvider is a kms.Provider that records nothing and wraps
// nothing — ResolveEncryption only ever calls KEKRef() on it.
type fakeProvider struct {
	ref  string
	cfg  map[string]any
	fips bool
}

func (p *fakeProvider) Name() string   { return "fake" }
func (p *fakeProvider) KEKRef() string { return p.ref }
func (p *fakeProvider) WrapDEK(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("not used")
}
func (p *fakeProvider) UnwrapDEK(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("not used")
}
func (p *fakeProvider) Shred(context.Context) error { return nil }
func (p *fakeProvider) FIPSMode() bool              { return p.fips }
func (p *fakeProvider) Close() error                { return nil }

// fakeRegistry returns a registry with a "fake-kms" scheme, plus a
// pointer to the last config map the builder saw so a test can assert
// the provider config actually reached the plugin.
func fakeRegistry(t *testing.T, openErr error) (*kms.Registry, *map[string]any) {
	t.Helper()
	var seen map[string]any
	r := kms.NewRegistry()
	r.Register("fake-kms", func(_ context.Context, ref string, cfg map[string]any) (kms.Provider, error) {
		seen = cfg
		if openErr != nil {
			return nil, openErr
		}
		return &fakeProvider{ref: ref, cfg: cfg}, nil
	})
	return r, &seen
}

// The local-custody matrix. The agent's schedule engine and the
// control-plane executor resolve encryption through this function;
// before a shared resolver existed they built TakeOptions with no
// Encryption at all — every scheduled backup was silently plaintext in
// an --encrypt repo, and plaintext-hash dedup then welded plaintext
// manifests onto encrypted chunks (crypto-shred guarantee broken) and
// vice versa (restore failure).
func TestResolveEncryption_LocalCustody(t *testing.T) {
	ctx := context.Background()

	t.Run("no_kek_means_plaintext", func(t *testing.T) {
		cfg, err := ResolveEncryption(ctx, EncryptionRequest{KeyringDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if cfg != nil {
			t.Fatalf("empty keyring: cfg = %+v, want nil", cfg)
		}
	})

	t.Run("kek_present_means_encrypt", func(t *testing.T) {
		dir := t.TempDir()
		// Materialise a KEK the way init --encrypt does.
		if _, generated, err := keystore.LoadOrGenerateKEK(dir); err != nil || !generated {
			t.Fatalf("generate KEK: generated=%v err=%v", generated, err)
		}
		cfg, err := ResolveEncryption(ctx, EncryptionRequest{KeyringDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if cfg == nil {
			t.Fatal("KEK present but resolver chose plaintext — scheduled backups would silently not encrypt")
		}
		if cfg.KEKRef != keystore.KEKRefLocal {
			t.Errorf("KEKRef = %q, want %q", cfg.KEKRef, keystore.KEKRefLocal)
		}
		var zero [32]byte
		if cfg.KEK == zero {
			t.Error("KEK is all-zero — keyring key was not actually loaded")
		}
	})

	t.Run("corrupt_kek_is_an_error_not_plaintext", func(t *testing.T) {
		dir := t.TempDir()
		// A present-but-invalid KEK must fail the backup loudly, never
		// silently fall back to plaintext.
		if err := os.WriteFile(filepath.Join(dir, keystore.KEKFileName), []byte("short"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ResolveEncryption(ctx, EncryptionRequest{KeyringDir: dir})
		if err == nil {
			t.Fatal("corrupt KEK accepted (or silently ignored) — want an error")
		}
		if !errors.Is(err, ErrKEKLoad) {
			t.Errorf("err = %v, want it to wrap ErrKEKLoad so the CLI can stamp backup.kek_load_failed", err)
		}
	})

	t.Run("explicit_local_ref_uses_keyring", func(t *testing.T) {
		dir := t.TempDir()
		if _, _, err := keystore.LoadOrGenerateKEK(dir); err != nil {
			t.Fatal(err)
		}
		// A deployment may pin kek_ref: local:default explicitly. That
		// must NOT be routed to the KMS registry (no "local" provider
		// is registered there), and a rotated local ref like local:v2
		// must behave the same.
		for _, ref := range []string{keystore.KEKRefLocal, "local:v2"} {
			cfg, err := ResolveEncryption(ctx, EncryptionRequest{KeyringDir: dir, KEKRef: ref})
			if err != nil {
				t.Fatalf("ref %q: %v", ref, err)
			}
			if cfg == nil || cfg.Provider != nil {
				t.Fatalf("ref %q: routed to a cloud provider; want the keyring path", ref)
			}
		}
	})
}

func TestResolveEncryption_Flags(t *testing.T) {
	ctx := context.Background()

	t.Run("conflicting_flags", func(t *testing.T) {
		_, err := ResolveEncryption(ctx, EncryptionRequest{
			KeyringDir: t.TempDir(), Encrypt: true, NoEncrypt: true,
		})
		if !errors.Is(err, ErrConflictingEncryptFlags) {
			t.Fatalf("err = %v, want ErrConflictingEncryptFlags", err)
		}
	})

	t.Run("no_encrypt_beats_a_configured_kek_ref", func(t *testing.T) {
		reg, _ := fakeRegistry(t, nil)
		// --no-encrypt is the operator's explicit override of a
		// deployment's kek_ref. It must short-circuit before the
		// provider is even opened, so an unreachable KMS can't block a
		// deliberately-plaintext run.
		cfg, err := ResolveEncryption(ctx, EncryptionRequest{
			KeyringDir: t.TempDir(),
			KEKRef:     "fake-kms://key",
			NoEncrypt:  true,
			Registry:   reg,
		})
		if err != nil {
			t.Fatal(err)
		}
		if cfg != nil {
			t.Fatalf("cfg = %+v, want nil (plaintext)", cfg)
		}
	})

	t.Run("encrypt_without_any_key_refuses", func(t *testing.T) {
		_, err := ResolveEncryption(ctx, EncryptionRequest{
			KeyringDir: t.TempDir(), Encrypt: true,
		})
		if !errors.Is(err, ErrEncryptNoKEK) {
			t.Fatalf("err = %v, want ErrEncryptNoKEK", err)
		}
	})
}

func TestResolveEncryption_CloudKMS(t *testing.T) {
	ctx := context.Background()

	t.Run("opens_provider_and_stamps_its_ref", func(t *testing.T) {
		reg, seen := fakeRegistry(t, nil)
		cfg, err := ResolveEncryption(ctx, EncryptionRequest{
			KeyringDir: t.TempDir(),
			KEKRef:     "fake-kms://acme/db1",
			KMSConfig:  map[string]any{"region": "eu-central-1"},
			Registry:   reg,
		})
		if err != nil {
			t.Fatal(err)
		}
		if cfg == nil || cfg.Provider == nil {
			t.Fatal("cloud KEKRef did not produce a provider-backed config")
		}
		if cfg.KEKRef != "fake-kms://acme/db1" {
			t.Errorf("KEKRef = %q, want the provider's own ref", cfg.KEKRef)
		}
		if got := (*seen)["region"]; got != "eu-central-1" {
			t.Errorf("provider config region = %v, want eu-central-1 — the kms.providers block never reached the plugin", got)
		}
	})

	t.Run("cloud_ref_wins_over_a_keyring_kek", func(t *testing.T) {
		dir := t.TempDir()
		if _, _, err := keystore.LoadOrGenerateKEK(dir); err != nil {
			t.Fatal(err)
		}
		reg, _ := fakeRegistry(t, nil)
		cfg, err := ResolveEncryption(ctx, EncryptionRequest{
			KeyringDir: dir, KEKRef: "fake-kms://acme/db1", Registry: reg,
		})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Provider == nil {
			t.Fatal("a host with a local kek.bin fell back to it despite a configured cloud kek_ref")
		}
	})

	t.Run("open_failure_never_falls_back", func(t *testing.T) {
		dir := t.TempDir()
		// A local KEK is present: the tempting-but-wrong behaviour is
		// to shrug and use it. That silently writes local-wrapped
		// manifests into a repo whose other manifests are KMS-wrapped.
		if _, _, err := keystore.LoadOrGenerateKEK(dir); err != nil {
			t.Fatal(err)
		}
		reg, _ := fakeRegistry(t, errors.New("credentials expired"))
		cfg, err := ResolveEncryption(ctx, EncryptionRequest{
			KeyringDir: dir, KEKRef: "fake-kms://acme/db1", Registry: reg,
		})
		if err == nil {
			t.Fatalf("provider open failed but resolver returned cfg = %+v", cfg)
		}
		if !errors.Is(err, ErrKMSOpen) {
			t.Errorf("err = %v, want it to wrap ErrKMSOpen", err)
		}
	})

	t.Run("unregistered_scheme_refuses", func(t *testing.T) {
		reg, _ := fakeRegistry(t, nil)
		_, err := ResolveEncryption(ctx, EncryptionRequest{
			KeyringDir: t.TempDir(), KEKRef: "no-such-kms://key", Registry: reg,
		})
		if !errors.Is(err, kms.ErrUnknownScheme) {
			t.Fatalf("err = %v, want it to wrap kms.ErrUnknownScheme", err)
		}
	})
}
