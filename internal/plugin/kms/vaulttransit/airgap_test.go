package vaulttransit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	stdkms "github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/kms/vaulttransit"
)

// TestBuilder_AirgapStrict pins the vault-transit half of H19: the
// Vault address from the KEKRef was dialled without consulting the
// air-gap policy.
func TestBuilder_AirgapStrict(t *testing.T) {
	airgap.LockForTest(t)
	defer airgap.WithScope(airgap.Policy{Mode: airgap.ModeStrict})()
	cfg := map[string]any{"vault_token": "t"}

	p, err := stdkms.DefaultRegistry.Open(context.Background(), "vault-transit://vault.example.com:8200/transit/k", cfg)
	if p != nil {
		_ = p.Close()
	}
	if !errors.Is(err, airgap.ErrEndpointNotAllowed) {
		t.Fatalf("public Vault under strict air-gap: Open = %v, want airgap.ErrEndpointNotAllowed", err)
	}

	p, err = stdkms.DefaultRegistry.Open(context.Background(), "vault-transit://10.0.0.5:8200/transit/k", cfg)
	if err != nil {
		t.Fatalf("RFC1918 Vault under strict air-gap: Open = %v, want allowed", err)
	}
	_ = p.Close()
}
