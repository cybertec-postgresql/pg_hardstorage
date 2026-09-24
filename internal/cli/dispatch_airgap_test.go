package cli

import (
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
)

// The operator-side control-plane client is an outbound endpoint like
// the agent's: under `airgapped: strict` a public --control-plane URL is
// refused before any request is made.
func TestNewDispatchClient_AirgapStrictRefusesPublicControlPlane(t *testing.T) {
	airgap.LockForTest(t)
	defer airgap.WithScope(airgap.Policy{Mode: airgap.ModeStrict})()

	if _, err := newDispatchClient(&dispatchAuthFlags{controlPlane: "https://cp.example.com"}); !errors.Is(err, airgap.ErrEndpointNotAllowed) {
		t.Fatalf("public control plane under strict air-gap: err = %v, want airgap.ErrEndpointNotAllowed", err)
	}
	if _, err := newDispatchClient(&dispatchAuthFlags{controlPlane: "http://10.0.0.5:8443"}); err != nil {
		t.Fatalf("RFC1918 control plane refused: %v", err)
	}
}
