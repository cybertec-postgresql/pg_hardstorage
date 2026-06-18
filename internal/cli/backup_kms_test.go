package cli

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// fakeKMSProvider mirrors the awskms shape for unit tests
// without a live AWS connection.
type fakeKMSProvider struct{ kekRef string }

func (f *fakeKMSProvider) Name() string                                          { return "fake-kms" }
func (f *fakeKMSProvider) KEKRef() string                                        { return f.kekRef }
func (f *fakeKMSProvider) WrapDEK(_ context.Context, dek []byte) ([]byte, error) { return dek, nil }
func (f *fakeKMSProvider) UnwrapDEK(_ context.Context, wrapped []byte) ([]byte, error) {
	return wrapped, nil
}
func (f *fakeKMSProvider) Shred(_ context.Context) error { return nil }
func (f *fakeKMSProvider) FIPSMode() bool                { return false }
func (f *fakeKMSProvider) Close() error                  { return nil }

// registerFakeKMS adds the "fake-kms" scheme to the default
// registry for the duration of t and removes it on cleanup.
// Note: the kms.Registry doesn't expose Unregister; we
// overwrite the scheme with a no-op builder in cleanup so
// other tests don't see a stray fake provider.
func registerFakeKMS(t *testing.T) {
	t.Helper()
	kms.DefaultRegistry.Register("fake-kms", func(_ context.Context, ref string, _ map[string]any) (kms.Provider, error) {
		return &fakeKMSProvider{kekRef: ref}, nil
	})
	t.Cleanup(func() {
		kms.DefaultRegistry.Register("fake-kms", func(_ context.Context, _ string, _ map[string]any) (kms.Provider, error) {
			return nil, errors.New("fake-kms: cleared")
		})
	})
}

func TestResolveBackupEncryption_CloudKMS_OpensProvider(t *testing.T) {
	registerFakeKMS(t)
	dir := t.TempDir()
	cfg, err := resolveBackupEncryption(context.Background(), dir, false, false,
		"fake-kms://my-prod-key", nil)
	if err != nil {
		t.Fatalf("resolveBackupEncryption: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil cfg for cloud-KMS path")
	}
	if cfg.Provider == nil {
		t.Fatal("expected non-nil Provider for cloud-KMS path")
	}
	if cfg.KEKRef != "fake-kms://my-prod-key" {
		t.Errorf("KEKRef = %q", cfg.KEKRef)
	}
	cfg.Provider.Close()
}

func TestResolveBackupEncryption_CloudKMS_NoLocalKEKRequired(t *testing.T) {
	registerFakeKMS(t)
	// dir is intentionally empty — the cloud-KMS path must
	// not fall through to the "encrypt without KEK" refusal.
	dir := t.TempDir()
	cfg, err := resolveBackupEncryption(context.Background(), dir,
		true, // --encrypt
		false,
		"fake-kms://x", nil)
	if err != nil {
		t.Fatalf("--encrypt + cloud KMS should succeed without local kek.bin: %v", err)
	}
	if cfg.Provider == nil {
		t.Fatal("expected Provider")
	}
	cfg.Provider.Close()
}

func TestResolveBackupEncryption_CloudKMS_ProviderOpenFailsBubbles(t *testing.T) {
	// Don't register the scheme — Open will fail with
	// kms.ErrUnknownScheme.
	dir := t.TempDir()
	_, err := resolveBackupEncryption(context.Background(), dir, false, false,
		"never-registered://x", nil)
	if err == nil {
		t.Fatal("expected error for unregistered scheme")
	}
	if !strings.Contains(err.Error(), "kms_open_failed") &&
		!strings.Contains(err.Error(), "open cloud KMS") {
		t.Errorf("error should mention KMS open failure; got %v", err)
	}
}

// TestResolveBackupEncryption_CloudKMS_UnreachableExits8 pins the backup-path
// extension of the kms.unreachable fix: when the cloud KMS endpoint is
// unreachable (the builder returns a network error), the backup encryption
// setup surfaces kms.unreachable → ExitUnreachable (8), not the generic
// backup.kms_open_failed → ExitError (1) it used to.
func TestResolveBackupEncryption_CloudKMS_UnreachableExits8(t *testing.T) {
	kms.DefaultRegistry.Register("unreachable-kms", func(_ context.Context, _ string, _ map[string]any) (kms.Provider, error) {
		return nil, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	})
	t.Cleanup(func() {
		kms.DefaultRegistry.Register("unreachable-kms", func(_ context.Context, _ string, _ map[string]any) (kms.Provider, error) {
			return nil, errors.New("cleared")
		})
	})

	dir := t.TempDir()
	_, err := resolveBackupEncryption(context.Background(), dir, false, false,
		"unreachable-kms://prod-key", nil)
	if err == nil {
		t.Fatal("expected error for unreachable cloud KMS")
	}
	if got := output.ExitCodeFor(err); got != output.ExitUnreachable {
		t.Errorf("unreachable KMS during backup: exit = %d, want %d (ExitUnreachable)", got, output.ExitUnreachable)
	}
	if oe, ok := output.AsOutputError(err); !ok || oe.Code != "kms.unreachable" {
		t.Errorf("want code kms.unreachable, got %v", oe)
	}
}

// TestResolveBackupEncryption_PassesKMSConfigToProvider proves the
// --kms-config flag value reaches the cloud KMS provider builder on the
// StringToStringVar path shared by backup / restore / partial — including
// stringMapToAny's "true"/"false" → bool coercion.
func TestResolveBackupEncryption_PassesKMSConfigToProvider(t *testing.T) {
	var gotCfg map[string]any
	kms.DefaultRegistry.Register("cfgrec", func(_ context.Context, ref string, cfg map[string]any) (kms.Provider, error) {
		gotCfg = cfg
		return &fakeKMSProvider{kekRef: ref}, nil
	})
	t.Cleanup(func() {
		kms.DefaultRegistry.Register("cfgrec", func(_ context.Context, _ string, _ map[string]any) (kms.Provider, error) {
			return nil, errors.New("cleared")
		})
	})

	dir := t.TempDir()
	cfg, err := resolveBackupEncryption(context.Background(), dir, false, false,
		"cfgrec://my-key", map[string]string{"region": "ap-southeast-2", "tls": "true"})
	if err != nil {
		t.Fatalf("resolveBackupEncryption: %v", err)
	}
	cfg.Provider.Close()

	if gotCfg["region"] != "ap-southeast-2" {
		t.Errorf("builder cfg region = %v, want ap-southeast-2", gotCfg["region"])
	}
	if gotCfg["tls"] != true {
		t.Errorf("builder cfg tls = %v (%T), want bool true (stringMapToAny coercion)", gotCfg["tls"], gotCfg["tls"])
	}
}

func TestResolveBackupEncryption_LocalKEKRefStillUsesLocal(t *testing.T) {
	// "local:default" must NOT be routed through the cloud
	// path.  Registering a "local" scheme in the registry
	// would normally cause confusion; the dispatcher checks
	// keystore.SchemeOf(kekRef) == "local" first.
	dir := t.TempDir()
	cfg, err := resolveBackupEncryption(context.Background(), dir, false, false,
		"local:default", nil)
	if err != nil {
		t.Fatal(err)
	}
	// dir is empty → no kek.bin → cfg should be nil (the
	// auto-detect fallback for "no encryption configured").
	if cfg != nil {
		t.Errorf("local:default with empty dir should yield nil cfg; got %+v", cfg)
	}
}

func TestStringMapToAny_TypeCoercion(t *testing.T) {
	in := map[string]string{
		"region":            "us-east-1",
		"use_fips_endpoint": "true",
		"insecure":          "FALSE",
		"empty":             "",
	}
	got := stringMapToAny(in)
	if got["region"] != "us-east-1" {
		t.Errorf("region = %v", got["region"])
	}
	if got["use_fips_endpoint"] != true {
		t.Errorf("true should coerce to bool true; got %T %v", got["use_fips_endpoint"], got["use_fips_endpoint"])
	}
	if got["insecure"] != false {
		t.Errorf("FALSE should coerce to bool false (case-insensitive); got %T %v", got["insecure"], got["insecure"])
	}
}
