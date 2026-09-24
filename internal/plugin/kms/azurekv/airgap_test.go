package azurekv_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	stdkms "github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/kms/azurekv"
)

// TestBuilder_AirgapStrictRefusesPublicVault pins the azure-kv half of
// H19: the provider opened a client for the public *.vault.azure.net
// endpoint without consulting the air-gap policy.
func TestBuilder_AirgapStrictRefusesPublicVault(t *testing.T) {
	airgap.LockForTest(t)
	defer airgap.WithScope(airgap.Policy{Mode: airgap.ModeStrict})()

	p, err := stdkms.DefaultRegistry.Open(context.Background(), "azure-kv://acme-vault/db-kek", nil)
	if p != nil {
		_ = p.Close()
	}
	if !errors.Is(err, airgap.ErrEndpointNotAllowed) {
		t.Fatalf("Open under strict air-gap = %v, want airgap.ErrEndpointNotAllowed", err)
	}
}
