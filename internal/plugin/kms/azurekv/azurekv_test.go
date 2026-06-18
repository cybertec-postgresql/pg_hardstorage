package azurekv_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	stdkms "github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/kms/azurekv"
)

// fakeAzureKV implements azurekv.Client without contacting
// the cloud.  Wrap prepends "AZ:"; Unwrap strips it.
type fakeAzureKV struct {
	wrapVersion   string
	wrapAlg       string
	wrapCipher    []byte
	unwrapVersion string
	unwrapAlg     string
	unwrapCipher  []byte
	unwrapPlain   []byte
	unwrapErr     error
	deleted       bool
	deleteErr     error
	describeBody  map[string]any
	closed        bool
}

func (f *fakeAzureKV) Wrap(_ context.Context, version, alg string, dek []byte) ([]byte, error) {
	f.wrapVersion = version
	f.wrapAlg = alg
	out := append([]byte("AZ:"), dek...)
	f.wrapCipher = out
	return out, nil
}

func (f *fakeAzureKV) Unwrap(_ context.Context, version, alg string, ct []byte) ([]byte, error) {
	f.unwrapVersion = version
	f.unwrapAlg = alg
	f.unwrapCipher = ct
	if f.unwrapErr != nil {
		return nil, f.unwrapErr
	}
	if !strings.HasPrefix(string(ct), "AZ:") {
		return nil, errors.New("unwrap: not our ciphertext")
	}
	return ct[len("AZ:"):], nil
}

func (f *fakeAzureKV) Delete(_ context.Context) error {
	f.deleted = true
	return f.deleteErr
}

func (f *fakeAzureKV) Describe(_ context.Context, version string) (map[string]any, error) {
	if f.describeBody != nil {
		return f.describeBody, nil
	}
	return map[string]any{
		"kid":     "https://acme-vault.vault.azure.net/keys/db-backup-kek/v1",
		"enabled": true,
	}, nil
}

func (f *fakeAzureKV) Close() error { f.closed = true; return nil }

const sampleKeyRef = "azure-kv://acme-vault/db-backup-kek"

func TestProvider_WrapUnwrapRoundTrip(t *testing.T) {
	cli := &fakeAzureKV{}
	p, err := azurekv.NewWithClient(sampleKeyRef, cli)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	dek := []byte("32-byte-aes-key-padded-here-OK")
	wrapped, err := p.WrapDEK(context.Background(), dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if cli.wrapAlg != azurekv.DefaultWrapAlgorithm {
		t.Errorf("Wrap alg = %q, want %q", cli.wrapAlg, azurekv.DefaultWrapAlgorithm)
	}
	got, err := p.UnwrapDEK(context.Background(), wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if string(got) != string(dek) {
		t.Errorf("round-trip lost bytes")
	}
}

func TestProvider_VersionPinnedKEKRef(t *testing.T) {
	cli := &fakeAzureKV{}
	ref := sampleKeyRef + "/abcdef0123456789"
	p, err := azurekv.NewWithClient(ref, cli)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	if p.KEKRef() != ref {
		t.Errorf("KEKRef = %q", p.KEKRef())
	}
	if _, err := p.WrapDEK(context.Background(), []byte("dek")); err != nil {
		t.Fatal(err)
	}
	if cli.wrapVersion != "abcdef0123456789" {
		t.Errorf("Wrap should pass version; got %q", cli.wrapVersion)
	}
}

func TestProvider_UnwrapErrorWrapsSentinel(t *testing.T) {
	cli := &fakeAzureKV{unwrapErr: errors.New("Forbidden")}
	p, _ := azurekv.NewWithClient(sampleKeyRef, cli)
	_, err := p.UnwrapDEK(context.Background(), []byte("anything"))
	if !errors.Is(err, stdkms.ErrUnwrap) {
		t.Errorf("expected ErrUnwrap wrapping; got %v", err)
	}
}

func TestProvider_Shred(t *testing.T) {
	cli := &fakeAzureKV{}
	p, _ := azurekv.NewWithClient(sampleKeyRef, cli)
	if err := p.Shred(context.Background()); err != nil {
		t.Fatalf("Shred: %v", err)
	}
	if !cli.deleted {
		t.Error("Shred should call DeleteKey")
	}
}

func TestProvider_ShredErrorWrapsSentinel(t *testing.T) {
	cli := &fakeAzureKV{deleteErr: errors.New("ConflictError")}
	p, _ := azurekv.NewWithClient(sampleKeyRef, cli)
	err := p.Shred(context.Background())
	if !errors.Is(err, stdkms.ErrShredFailed) {
		t.Errorf("expected ErrShredFailed; got %v", err)
	}
}

func TestProvider_DescribeKey(t *testing.T) {
	cli := &fakeAzureKV{}
	p, _ := azurekv.NewWithClient(sampleKeyRef, cli)
	body, err := p.DescribeKey(context.Background())
	if err != nil {
		t.Fatalf("DescribeKey: %v", err)
	}
	if body["enabled"] != true {
		t.Errorf("enabled = %v", body["enabled"])
	}
	if body["vault_url"] != "https://acme-vault.vault.azure.net/" {
		t.Errorf("vault_url = %v", body["vault_url"])
	}
	if body["key_name"] != "db-backup-kek" {
		t.Errorf("key_name = %v", body["key_name"])
	}
}

func TestProvider_RejectsBadKEKRef(t *testing.T) {
	cli := &fakeAzureKV{}
	cases := []string{
		"local:default",         // wrong scheme
		"azure-kv://",           // empty
		"azure-kv://onlyvault",  // missing key
		"azure-kv://vault/",     // empty key
		"azure-kv://vault/key/", // empty version
	}
	for _, kr := range cases {
		_, err := azurekv.NewWithClient(kr, cli)
		if err == nil {
			t.Errorf("expected error for %q", kr)
		}
	}
}

func TestProvider_SovereignCloud(t *testing.T) {
	cli := &fakeAzureKV{}
	// Dotted vault → literal hostname (us-gov / china /
	// custom domain).
	p, err := azurekv.NewWithClient("azure-kv://acme-vault.vault.azure.cn/key", cli)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := p.DescribeKey(context.Background())
	if body["vault_url"] != "https://acme-vault.vault.azure.cn/" {
		t.Errorf("sovereign-cloud vault URL = %v", body["vault_url"])
	}
}

func TestProvider_FIPSMode(t *testing.T) {
	cli := &fakeAzureKV{}
	p, _ := azurekv.NewWithClient(sampleKeyRef, cli, azurekv.WithFIPSMode(true))
	if !p.FIPSMode() {
		t.Error("FIPSMode option not honoured")
	}
}

func TestProvider_WrapAlgorithmOverride(t *testing.T) {
	cli := &fakeAzureKV{}
	p, _ := azurekv.NewWithClient(sampleKeyRef, cli, azurekv.WithWrapAlgorithm("RSA-OAEP"))
	if _, err := p.WrapDEK(context.Background(), []byte("dek")); err != nil {
		t.Fatal(err)
	}
	if cli.wrapAlg != "RSA-OAEP" {
		t.Errorf("override not honoured; got %q", cli.wrapAlg)
	}
}

func TestProvider_ClosedRefuses(t *testing.T) {
	cli := &fakeAzureKV{}
	p, _ := azurekv.NewWithClient(sampleKeyRef, cli)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.WrapDEK(context.Background(), []byte("x")); err == nil {
		t.Error("WrapDEK on closed provider should fail")
	}
	if !cli.closed {
		t.Error("Close should propagate to the underlying client")
	}
}

func TestProvider_RegistryRoundTrip(t *testing.T) {
	schemes := stdkms.DefaultRegistry.Schemes()
	found := false
	for _, s := range schemes {
		if s == azurekv.Scheme {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("azure-kv not registered; schemes=%v", schemes)
	}
}
