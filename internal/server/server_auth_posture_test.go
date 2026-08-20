package server

// SEC-1: a control plane reachable from beyond loopback must
// authenticate its clients — bearer token or mTLS client
// verification. requireAuth skips token checking when no token is
// configured (documented for behind-mTLS deployments), so without a
// construction-time gate a non-loopback listener with plain TLS and
// no client auth exposes the full job-queue + restore API (arbitrary
// target_dir materialization on every agent) to anyone on the
// network.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNew_NonLoopbackRequiresAuth(t *testing.T) {
	const nonLoopback = "10.99.99.9:8443" // unroutable: New must not bind

	t.Run("refused without any auth", func(t *testing.T) {
		if _, err := New(Config{Listen: nonLoopback}); err == nil {
			t.Fatal("New: unauthenticated non-loopback control plane must be refused")
		} else if !strings.Contains(err.Error(), "requires authentication") {
			t.Errorf("error %q does not explain the auth requirement", err)
		}
	})

	t.Run("refused with server TLS but no client auth or token", func(t *testing.T) {
		_, err := New(Config{
			Listen: nonLoopback,
			TLS:    TLSConfig{CertFile: "cert.pem", KeyFile: "key.pem"},
		})
		if err == nil {
			t.Fatal("New: TLS without mTLS client auth or a token is still unauthenticated")
		} else if !strings.Contains(err.Error(), "requires authentication") {
			t.Errorf("error %q does not explain the auth requirement", err)
		}
	})

	t.Run("allowed with a bearer token", func(t *testing.T) {
		dir := t.TempDir()
		tokenFile := filepath.Join(dir, "token")
		if err := os.WriteFile(tokenFile, []byte("secret-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := New(Config{Listen: nonLoopback, Auth: AuthConfig{TokenFile: tokenFile}})
		if err != nil {
			t.Fatalf("New with token: %v", err)
		}
		if s.token != "secret-token" {
			t.Errorf("token = %q", s.token)
		}
	})

	t.Run("allowed with mTLS client verification configured", func(t *testing.T) {
		// New does not load the CA (Run does), so the gate only needs
		// the client-verification posture to be configured.
		_, err := New(Config{
			Listen: nonLoopback,
			TLS: TLSConfig{
				CertFile:     "cert.pem",
				KeyFile:      "key.pem",
				ClientCAFile: "client-ca.pem",
			},
		})
		if err != nil {
			t.Fatalf("New with mTLS configured: %v", err)
		}
	})

	t.Run("loopback without auth stays allowed (local DX default)", func(t *testing.T) {
		if _, err := New(Config{Listen: "127.0.0.1:0"}); err != nil {
			t.Fatalf("loopback default must keep working: %v", err)
		}
		if _, err := New(Config{}); err != nil {
			t.Fatalf("empty Listen (defaults to 127.0.0.1:8443) must keep working: %v", err)
		}
	})
}
