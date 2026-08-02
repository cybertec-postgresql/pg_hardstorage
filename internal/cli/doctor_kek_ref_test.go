package cli

// A deployment can declare a kek_ref whose scheme this binary doesn't
// link — a typo, or a pkcs11:// ref on a build without the pkcs11 tag.
// Config load deliberately accepts it (registration is a CLI-layer
// init() and flavour-dependent, so `lint` must not depend on it), which
// leaves doctor as the place an operator finds out before the first
// backup fails at provider-open time.

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

func TestAppendKEKRefChecks(t *testing.T) {
	loaded := func(deps map[string]config.DeploymentConfig) *config.LoadResult {
		return &config.LoadResult{Config: config.Config{Deployments: deps}}
	}

	t.Run("flags_an_unregistered_scheme", func(t *testing.T) {
		issues := appendKEKRefChecks(loaded(map[string]config.DeploymentConfig{
			"db1": {KEKRef: "aws-kmz://alias/typo"},
		}), nil)
		if len(issues) != 1 {
			t.Fatalf("issues = %+v, want exactly one", issues)
		}
		if issues[0].Code != "config.kek_ref_unknown_scheme" {
			t.Errorf("code = %q", issues[0].Code)
		}
		if !strings.Contains(issues[0].Message, "db1") {
			t.Errorf("message should name the deployment: %q", issues[0].Message)
		}
	})

	t.Run("accepts_local_and_registered_schemes", func(t *testing.T) {
		registerFakeKMS(t)
		issues := appendKEKRefChecks(loaded(map[string]config.DeploymentConfig{
			"db1": {KEKRef: "local:default"},
			"db2": {KEKRef: "fake-kms://acme/key"},
			"db3": {}, // no kek_ref at all
		}), nil)
		if len(issues) != 0 {
			t.Errorf("issues = %+v, want none", issues)
		}
	})

	t.Run("reports_every_offending_deployment", func(t *testing.T) {
		issues := appendKEKRefChecks(loaded(map[string]config.DeploymentConfig{
			"db1": {KEKRef: "nope-a://x"},
			"db2": {KEKRef: "nope-b://y"},
		}), nil)
		if len(issues) != 2 {
			t.Fatalf("issues = %d, want 2 — a fleet-wide typo shouldn't hide behind the first hit", len(issues))
		}
	})
}
