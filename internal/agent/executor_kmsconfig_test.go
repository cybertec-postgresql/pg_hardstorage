package agent

// executor_kmsconfig_test.go — the agent's restore and verify paths
// must reach a cloud KMS.
//
// This is the gap issue #44 was actually about. `restore` and `verify`
// take --kms-config on the command line, but the AGENT builds its own
// restore and verify internally and passes no flags: its only source of
// provider configuration is the `kms:` block in the config file. Until
// that was wired, a deployment whose provider needs an explicit region
// or endpoint could be backed up by the agent and then never restored
// or verified by it.
//
// Both executors' unwrapDEK sat at 0% coverage — the two functions
// that decide whether the fix works at all.
//
// They are driven from ONE table. The two are byte-identical today and
// must stay that way; a shared table turns a divergence into a failing
// case instead of a file nobody wrote. In-package, because unwrapDEK is
// unexported and its exported callers need a whole restore or verify
// run to reach.

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
)

// unwrapper is one executor's DEK-unwrapping entry point.
type unwrapper func(ctx context.Context, kekRef string, wrapped []byte) ([]byte, error)

// executorsUnderTest builds both executors over the same config. The
// verifier is nil: unwrapDEK reads only the keyring dir and the KMS
// config, so a real one would add setup without adding coverage.
func executorsUnderTest(kmsCfg config.KMSConfig, keyringDir string) []struct {
	name   string
	unwrap unwrapper
} {
	deps := map[string]config.DeploymentConfig{}
	return []struct {
		name   string
		unwrap unwrapper
	}{
		{"restore", NewRestoreExecutor(deps, kmsCfg, nil, keyringDir).unwrapDEK},
		{"verify", NewVerifyExecutor(deps, kmsCfg, nil, keyringDir).unwrapDEK},
	}
}

// agentTestDEK is 32 bytes: keystore.UnwrapDEK enforces the AES-256
// key length on whatever a provider returns, so a shorter stub would
// fail for the wrong reason.
func agentTestDEK() []byte {
	dek := make([]byte, 32)
	for i := range dek {
		dek[i] = byte(i + 7)
	}
	return dek
}

type agentStubProvider struct{ ref string }

func (p *agentStubProvider) Name() string   { return "agent-kms" }
func (p *agentStubProvider) KEKRef() string { return p.ref }
func (p *agentStubProvider) WrapDEK(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("not used")
}
func (p *agentStubProvider) UnwrapDEK(context.Context, []byte) ([]byte, error) {
	return agentTestDEK(), nil
}
func (p *agentStubProvider) Shred(context.Context) error { return nil }
func (p *agentStubProvider) FIPSMode() bool              { return false }
func (p *agentStubProvider) Close() error                { return nil }

// registerFakeKMS installs a builder that records the config it was
// handed, and unregisters it afterwards. The scheme is unique per test
// so concurrently-running tests in this package cannot collide on the
// process-wide registry.
func registerFakeKMS(t *testing.T, scheme string) func() map[string]any {
	t.Helper()
	var got map[string]any
	kms.DefaultRegistry.Register(scheme,
		func(_ context.Context, ref string, cfg map[string]any) (kms.Provider, error) {
			got = cfg
			return &agentStubProvider{ref: ref}, nil
		})
	t.Cleanup(func() {
		kms.DefaultRegistry.Register(scheme,
			func(_ context.Context, _ string, _ map[string]any) (kms.Provider, error) {
				return nil, errors.New("cleared")
			})
	})
	return func() map[string]any { return got }
}

// TestExecutors_PassDeclaredProviderConfig is the regression test for
// the agent half of issue #44.
func TestExecutors_PassDeclaredProviderConfig(t *testing.T) {
	const scheme = "agent-kms-declared"
	const ref = scheme + "://vault/db1-kek"

	kmsCfg := config.KMSConfig{Providers: []config.KMSProvider{
		{KEKRef: ref, Config: map[string]any{"region": "eu-central-1"}},
	}}

	for _, e := range executorsUnderTest(kmsCfg, t.TempDir()) {
		t.Run(e.name, func(t *testing.T) {
			gotCfg := registerFakeKMS(t, scheme)

			dek, err := e.unwrap(context.Background(), ref, []byte("wrapped"))
			if err != nil {
				t.Fatalf("unwrapDEK: %v", err)
			}
			if string(dek) != string(agentTestDEK()) {
				t.Errorf("dek = %x, want the provider's own bytes", dek)
			}
			if got := gotCfg(); got["region"] != "eu-central-1" {
				t.Errorf("provider was opened with cfg %v; the kms.providers entry never "+
					"reached it. The agent passes no --kms-config — the config file is its "+
					"only channel — so this deployment could be backed up and then never "+
					"%sd by the agent", got, e.name)
			}
		})
	}
}

// TestExecutors_ResolveConfigPerKEKRef pins that the lookup is keyed on
// the MANIFEST's ref rather than on anything deployment-scoped.
//
// A repo holds manifests from several KEK references — that is the
// normal state during and after a rotation. Resolving one deployment's
// config for every manifest would send the wrong region to the
// provider, and the failure would look like a KMS outage.
func TestExecutors_ResolveConfigPerKEKRef(t *testing.T) {
	const scheme = "agent-kms-perref"
	kmsCfg := config.KMSConfig{Providers: []config.KMSProvider{
		{KEKRef: scheme + "://vault/old", Config: map[string]any{"region": "us-east-1"}},
		{KEKRef: scheme + "://vault/new", Config: map[string]any{"region": "eu-west-3"}},
	}}

	for _, e := range executorsUnderTest(kmsCfg, t.TempDir()) {
		t.Run(e.name, func(t *testing.T) {
			for _, tc := range []struct{ ref, want string }{
				{scheme + "://vault/old", "us-east-1"},
				{scheme + "://vault/new", "eu-west-3"},
			} {
				gotCfg := registerFakeKMS(t, scheme)
				if _, err := e.unwrap(context.Background(), tc.ref, []byte("wrapped")); err != nil {
					t.Fatalf("%s: unwrapDEK: %v", tc.ref, err)
				}
				if got := gotCfg(); got["region"] != tc.want {
					t.Errorf("%s opened with region %v, want %q — config must be resolved "+
						"from the manifest's own KEKRef, since one repo holds manifests "+
						"under several refs during a rotation", tc.ref, got["region"], tc.want)
				}
			}
		})
	}
}

// TestExecutors_UndeclaredRefStillOpens covers the common deployment:
// no `kms:` block at all, because the provider runs on ambient cloud
// credentials or needs no configuration.
//
// A nil config must reach the builder as nil and still work. Turning
// "nothing declared" into an error would break every deployment that
// never needed a kms: block.
func TestExecutors_UndeclaredRefStillOpens(t *testing.T) {
	const scheme = "agent-kms-undeclared"

	// A provider entry that does NOT match the ref being unwrapped.
	kmsCfg := config.KMSConfig{Providers: []config.KMSProvider{
		{KEKRef: scheme + "://vault/other", Config: map[string]any{"region": "ap-south-1"}},
	}}

	for _, e := range executorsUnderTest(kmsCfg, t.TempDir()) {
		t.Run(e.name, func(t *testing.T) {
			gotCfg := registerFakeKMS(t, scheme)

			if _, err := e.unwrap(context.Background(), scheme+"://vault/unlisted",
				[]byte("wrapped")); err != nil {
				t.Fatalf("unwrapDEK with no matching provider entry: %v\nA deployment on "+
					"ambient credentials declares no kms: block at all", err)
			}
			if got := gotCfg(); got != nil {
				t.Errorf("builder received cfg %v for an undeclared ref, want nil — another "+
					"entry's settings must not leak onto an unrelated KEKRef", got)
			}
		})
	}
}

// TestExecutors_LocalRefUsesKeyringNotKMS pins the routing split.
//
// A local ref must never reach the KMS registry. When it did, rotated
// backups stamped `local:v2` returned ErrUnknownScheme and were
// unrestorable — so this asserts the fake provider is NOT consulted.
func TestExecutors_LocalRefUsesKeyringNotKMS(t *testing.T) {
	for _, e := range executorsUnderTest(config.KMSConfig{}, t.TempDir()) {
		t.Run(e.name, func(t *testing.T) {
			// No keyring exists in the temp dir, so this fails — but it
			// must fail as a KEYRING error, not as an unknown KMS scheme.
			_, err := e.unwrap(context.Background(), "local:v2", []byte("wrapped"))
			if err == nil {
				t.Fatal("unwrapping against an empty keyring succeeded")
			}
			if errors.Is(err, kms.ErrUnknownScheme) {
				t.Errorf("a local: ref was routed to the KMS registry (%v); rotated "+
					"backups stamped local:v2 would be unrestorable", err)
			}
		})
	}
}
