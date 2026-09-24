package agent

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
)

// countingTransport records whether any request left the process.
type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, errors.New("network disabled in test")
}

// TestControlPlaneClient_AirgapStrictRefusesPublicURL pins that strict
// air-gap mode gates the control-plane URL like every other outbound
// endpoint: a public host is refused before a single request is sent.
func TestControlPlaneClient_AirgapStrictRefusesPublicURL(t *testing.T) {
	airgap.LockForTest(t)
	defer airgap.WithScope(airgap.Policy{Mode: airgap.ModeStrict})()

	tr := &countingTransport{}
	c := &ControlPlaneClient{
		BaseURL:     "https://control.example.com:8443",
		AgentID:     "a1",
		Deployments: []string{"db1"},
		HTTPClient:  &http.Client{Transport: tr},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.Run(ctx)
	if !errors.Is(err, airgap.ErrEndpointNotAllowed) {
		t.Fatalf("Run err = %v, want airgap.ErrEndpointNotAllowed", err)
	}
	if n := tr.n.Load(); n != 0 {
		t.Errorf("%d request(s) reached the transport; want none", n)
	}
}

// TestControlPlaneClient_AirgapStrictAllowsPrivateURL: a private-range
// control plane is fine under strict mode (Run proceeds to its loop).
func TestControlPlaneClient_AirgapStrictAllowsPrivateURL(t *testing.T) {
	airgap.LockForTest(t)
	defer airgap.WithScope(airgap.Policy{Mode: airgap.ModeStrict})()

	c := &ControlPlaneClient{
		BaseURL:     "https://10.0.0.5:8443",
		AgentID:     "a1",
		Deployments: []string{"db1"},
		HTTPClient:  &http.Client{Transport: &countingTransport{}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run err = %v, want context deadline (loop ran)", err)
	}
}
