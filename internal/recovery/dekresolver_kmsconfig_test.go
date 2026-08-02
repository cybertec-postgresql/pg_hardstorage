package recovery_test

// A recovery drill restores a backup to prove it is restorable, and
// `doctor` escalates to CRITICAL when a deployment's last passing drill
// goes stale. So a drill that cannot unwrap its DEK doesn't just fail
// quietly — it reports the deployment's backups as unproven.
//
// KeystoreDEKResolver used to hardcode a nil provider config, which
// meant a deployment whose KMS provider needs an explicit region or
// credential could never drill: the flags exist on `restore`, but a
// drill builds its own restore internally and takes none (issue #44).

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/recovery"
)

type stubProvider struct {
	ref string
	dek []byte
}

// testDEK is a 32-byte DEK — keystore.UnwrapDEK enforces the
// AES-256 key length on whatever a provider hands back.
func testDEK() []byte {
	dek := make([]byte, 32)
	for i := range dek {
		dek[i] = byte(i + 1)
	}
	return dek
}

func (p *stubProvider) Name() string   { return "drill-kms" }
func (p *stubProvider) KEKRef() string { return p.ref }
func (p *stubProvider) WrapDEK(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("not used")
}
func (p *stubProvider) UnwrapDEK(_ context.Context, _ []byte) ([]byte, error) {
	return p.dek, nil
}
func (p *stubProvider) Shred(context.Context) error { return nil }
func (p *stubProvider) FIPSMode() bool              { return false }
func (p *stubProvider) Close() error                { return nil }

func TestKeystoreDEKResolver_PassesProviderConfig(t *testing.T) {
	var gotCfg map[string]any
	var gotRef string
	kms.DefaultRegistry.Register("drill-kms", func(_ context.Context, ref string, cfg map[string]any) (kms.Provider, error) {
		gotCfg = cfg
		return &stubProvider{ref: ref, dek: testDEK()}, nil
	})
	t.Cleanup(func() {
		kms.DefaultRegistry.Register("drill-kms", func(_ context.Context, _ string, _ map[string]any) (kms.Provider, error) {
			return nil, errors.New("cleared")
		})
	})

	lookup := func(kekRef string) map[string]any {
		gotRef = kekRef
		return map[string]any{"region": "eu-central-1"}
	}

	resolve := recovery.KeystoreDEKResolver(t.TempDir(), lookup)
	dek, err := resolve(context.Background(), "drill-kms://vault/db1", []byte("wrapped"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if string(dek) != string(testDEK()) {
		t.Errorf("dek = %x, want the provider's own bytes", dek)
	}
	if gotRef != "drill-kms://vault/db1" {
		t.Errorf("lookup keyed on %q — provider config must be resolved per manifest KEKRef", gotRef)
	}
	if gotCfg["region"] != "eu-central-1" {
		t.Errorf("builder cfg = %v; the kms.providers entry never reached the provider, so a drill of a "+
			"region-scoped deployment would fail and report its backups unproven", gotCfg)
	}
}

// A nil lookup is the legitimate "no kms: block declared" case and must
// keep working — plenty of deployments run on ambient cloud credentials
// or a local KEK.
func TestKeystoreDEKResolver_NilLookupIsSafe(t *testing.T) {
	kms.DefaultRegistry.Register("drill-kms-nil", func(_ context.Context, ref string, cfg map[string]any) (kms.Provider, error) {
		if cfg != nil {
			t.Errorf("cfg = %v, want nil", cfg)
		}
		return &stubProvider{ref: ref, dek: testDEK()}, nil
	})
	t.Cleanup(func() {
		kms.DefaultRegistry.Register("drill-kms-nil", func(_ context.Context, _ string, _ map[string]any) (kms.Provider, error) {
			return nil, errors.New("cleared")
		})
	})

	resolve := recovery.KeystoreDEKResolver(t.TempDir(), nil)
	if _, err := resolve(context.Background(), "drill-kms-nil://vault/db1", []byte("wrapped")); err != nil {
		t.Fatalf("nil lookup: %v", err)
	}
}
