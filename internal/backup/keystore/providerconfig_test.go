package keystore_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
)

// TestUnwrapDEK_PassesProviderConfig proves the cloud-KMS provider builder
// receives the ProviderConfig map (the value behind --kms-config). Without
// this the flag would be silently dropped between the CLI and the provider.
func TestUnwrapDEK_PassesProviderConfig(t *testing.T) {
	reg := kms.NewRegistry()
	var gotCfg map[string]any
	reg.Register("cfgprobe", func(_ context.Context, ref string, cfg map[string]any) (kms.Provider, error) {
		gotCfg = cfg
		return &fakeProvider{kekRef: ref}, nil
	})

	dek := make([]byte, encryption.KeyLen)
	rand.Read(dek)
	wrapped := append([]byte("wrap-"), dek...) // fakeProvider's wrap shape

	out, err := keystore.UnwrapDEK(context.Background(), "cfgprobe://key", wrapped, keystore.UnwrapOpts{
		Registry:       reg,
		ProviderConfig: map[string]any{"region": "eu-central-1", "endpoint": "https://kms.local"},
	})
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if !bytes.Equal(out, dek) {
		t.Error("unwrap lost bytes")
	}
	if gotCfg["region"] != "eu-central-1" || gotCfg["endpoint"] != "https://kms.local" {
		t.Errorf("provider builder cfg = %v, want region=eu-central-1 endpoint=https://kms.local", gotCfg)
	}
}

// TestDEKResolver_PassesProviderConfig proves the shared DEKResolver (wired
// into restore.Options.UnwrapDEK from the CLI flag) forwards its
// providerConfig to the provider builder.
func TestDEKResolver_PassesProviderConfig(t *testing.T) {
	var gotCfg map[string]any
	kms.DefaultRegistry.Register("cfgprobe2", func(_ context.Context, ref string, cfg map[string]any) (kms.Provider, error) {
		gotCfg = cfg
		return &fakeProvider{kekRef: ref}, nil
	})
	t.Cleanup(func() {
		kms.DefaultRegistry.Register("cfgprobe2", func(_ context.Context, _ string, _ map[string]any) (kms.Provider, error) {
			return nil, context.Canceled
		})
	})

	dek := make([]byte, encryption.KeyLen)
	rand.Read(dek)
	wrapped := append([]byte("wrap-"), dek...)

	resolve := keystore.DEKResolver("", map[string]any{"region": "us-east-1"})
	out, err := resolve(context.Background(), "cfgprobe2://key", wrapped)
	if err != nil {
		t.Fatalf("DEKResolver: %v", err)
	}
	if !bytes.Equal(out, dek) {
		t.Error("unwrap lost bytes")
	}
	if gotCfg["region"] != "us-east-1" {
		t.Errorf("provider builder cfg = %v, want region=us-east-1", gotCfg)
	}
}
