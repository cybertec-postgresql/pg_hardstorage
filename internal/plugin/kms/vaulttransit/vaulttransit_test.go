package vaulttransit_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	stdkms "github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/kms/vaulttransit"
)

// fakeVault implements vaulttransit.Client without
// contacting a real Vault server.  Encrypt prepends
// `vault:v1:` and base64-encodes; Decrypt strips and
// decodes.
type fakeVault struct {
	encryptMount, encryptKey string
	decryptMount, decryptKey string
	deleteCalled             bool
	deleteErr                error
	encryptErr               error
	decryptErr               error
	readErr                  error
	closed                   bool
}

func (f *fakeVault) Encrypt(_ context.Context, mount, name, plaintext string) (string, error) {
	f.encryptMount = mount
	f.encryptKey = name
	if f.encryptErr != nil {
		return "", f.encryptErr
	}
	// Vault's real ciphertext format: `vault:v<n>:<base64>`.
	// We use v1 plus the same plaintext to keep the fake
	// trivially round-trippable.
	return "vault:v1:" + plaintext, nil
}

func (f *fakeVault) Decrypt(_ context.Context, mount, name, ciphertext string) (string, error) {
	f.decryptMount = mount
	f.decryptKey = name
	if f.decryptErr != nil {
		return "", f.decryptErr
	}
	if !strings.HasPrefix(ciphertext, "vault:v1:") {
		return "", errors.New("decrypt: not our ciphertext")
	}
	return strings.TrimPrefix(ciphertext, "vault:v1:"), nil
}

func (f *fakeVault) DeleteKey(_ context.Context, mount, name string) error {
	f.deleteCalled = true
	return f.deleteErr
}

func (f *fakeVault) ReadKey(_ context.Context, mount, name string) (map[string]any, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return map[string]any{
		"deletion_allowed":       false,
		"latest_version":         3,
		"min_decryption_version": 1,
	}, nil
}

func (f *fakeVault) Close() error { f.closed = true; return nil }

const sampleKeyRef = "vault-transit://vault.acme.example.com:8200/transit/db-kek"

func TestProvider_WrapUnwrapRoundTrip(t *testing.T) {
	cli := &fakeVault{}
	p, err := vaulttransit.NewWithClient(sampleKeyRef, cli)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	dek := []byte("32-byte-aes-key-padded-here-OK")
	wrapped, err := p.WrapDEK(context.Background(), dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if cli.encryptMount != "transit" || cli.encryptKey != "db-kek" {
		t.Errorf("Encrypt routed wrong: mount=%q name=%q", cli.encryptMount, cli.encryptKey)
	}
	// Wrapped form is the literal `vault:v1:...` string —
	// what production code stamps on the manifest.
	if !strings.HasPrefix(string(wrapped), "vault:v1:") {
		t.Errorf("wrapped form should keep the vault: prefix; got %q", wrapped)
	}
	got, err := p.UnwrapDEK(context.Background(), wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if string(got) != string(dek) {
		t.Errorf("round-trip lost bytes: got %q want %q", got, dek)
	}
}

func TestProvider_MultiSegmentMount(t *testing.T) {
	cli := &fakeVault{}
	ref := "vault-transit://10.0.0.5:8200/secrets-eu/transit/db-prod-kek"
	p, err := vaulttransit.NewWithClient(ref, cli)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	if _, err := p.WrapDEK(context.Background(), []byte("dek")); err != nil {
		t.Fatal(err)
	}
	if cli.encryptMount != "secrets-eu/transit" {
		t.Errorf("multi-segment mount lost; got %q", cli.encryptMount)
	}
	if cli.encryptKey != "db-prod-kek" {
		t.Errorf("key extraction wrong; got %q", cli.encryptKey)
	}
}

func TestProvider_UnwrapErrorWrapsSentinel(t *testing.T) {
	cli := &fakeVault{decryptErr: errors.New("permission denied")}
	p, _ := vaulttransit.NewWithClient(sampleKeyRef, cli)
	_, err := p.UnwrapDEK(context.Background(), []byte("vault:v1:anything"))
	if !errors.Is(err, stdkms.ErrUnwrap) {
		t.Errorf("expected ErrUnwrap wrapping; got %v", err)
	}
}

func TestProvider_UnwrapBadBase64WrapsSentinel(t *testing.T) {
	// fakeVault.Decrypt strips the prefix and returns the
	// remainder verbatim — which here isn't valid base64.
	cli := &fakeVault{}
	p, _ := vaulttransit.NewWithClient(sampleKeyRef, cli)
	_, err := p.UnwrapDEK(context.Background(), []byte("vault:v1:!!not-b64!!"))
	if !errors.Is(err, stdkms.ErrUnwrap) {
		t.Errorf("expected ErrUnwrap on bad base64; got %v", err)
	}
}

func TestProvider_Shred(t *testing.T) {
	cli := &fakeVault{}
	p, _ := vaulttransit.NewWithClient(sampleKeyRef, cli)
	if err := p.Shred(context.Background()); err != nil {
		t.Fatalf("Shred: %v", err)
	}
	if !cli.deleteCalled {
		t.Error("Shred should call DeleteKey")
	}
}

func TestProvider_ShredErrorWrapsSentinel(t *testing.T) {
	cli := &fakeVault{deleteErr: errors.New("deletion not allowed")}
	p, _ := vaulttransit.NewWithClient(sampleKeyRef, cli)
	err := p.Shred(context.Background())
	if !errors.Is(err, stdkms.ErrShredFailed) {
		t.Errorf("expected ErrShredFailed; got %v", err)
	}
}

func TestProvider_DescribeKey(t *testing.T) {
	cli := &fakeVault{}
	p, _ := vaulttransit.NewWithClient(sampleKeyRef, cli)
	body, err := p.DescribeKey(context.Background())
	if err != nil {
		t.Fatalf("DescribeKey: %v", err)
	}
	if body["mount"] != "transit" {
		t.Errorf("mount = %v", body["mount"])
	}
	if body["name"] != "db-kek" {
		t.Errorf("name = %v", body["name"])
	}
	if body["latest_version"] != 3 {
		t.Errorf("latest_version surfacing wrong: %v", body["latest_version"])
	}
}

func TestProvider_HTTPSchemePrefix(t *testing.T) {
	// `http+host` triggers the http:// scheme (in-cluster
	// Vault without TLS).
	cli := &fakeVault{}
	ref := "vault-transit://http+vault.internal:8200/transit/k"
	p, err := vaulttransit.NewWithClient(ref, cli)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	body, _ := p.DescribeKey(context.Background())
	if body["address"] != "http://vault.internal:8200" {
		t.Errorf("http+ scheme not honoured; addr = %v", body["address"])
	}
}

func TestProvider_RejectsBadKEKRef(t *testing.T) {
	cli := &fakeVault{}
	cases := []string{
		"local:default",                  // wrong scheme
		"vault-transit://",               // empty
		"vault-transit://hostonly",       // no path
		"vault-transit://host/onlymount", // mount but no key
		"vault-transit://host//key",      // empty path segment
		"vault-transit:///mount/key",     // empty host
	}
	for _, kr := range cases {
		_, err := vaulttransit.NewWithClient(kr, cli)
		if err == nil {
			t.Errorf("expected error for %q", kr)
		}
	}
}

func TestProvider_FIPSMode(t *testing.T) {
	cli := &fakeVault{}
	p, _ := vaulttransit.NewWithClient(sampleKeyRef, cli, vaulttransit.WithFIPSMode(true))
	if !p.FIPSMode() {
		t.Error("FIPSMode option not honoured")
	}
}

func TestProvider_ClosedRefuses(t *testing.T) {
	cli := &fakeVault{}
	p, _ := vaulttransit.NewWithClient(sampleKeyRef, cli)
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
		if s == vaulttransit.Scheme {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("vault-transit not registered; schemes=%v", schemes)
	}
}

// TestWrappedFormCarriesPrefix asserts the `vault:v<n>:`
// prefix survives the manifest round-trip.  The prefix is
// what tells Vault which key version was used at encrypt
// time; restore depends on it.
func TestWrappedFormCarriesPrefix(t *testing.T) {
	cli := &fakeVault{}
	p, _ := vaulttransit.NewWithClient(sampleKeyRef, cli)
	wrapped, _ := p.WrapDEK(context.Background(), []byte("dek-bytes"))
	if !strings.HasPrefix(string(wrapped), "vault:v1:") {
		t.Errorf("wrapped form must keep the prefix; got %q", wrapped)
	}
	// Sanity: the body after the prefix is base64.
	body := strings.TrimPrefix(string(wrapped), "vault:v1:")
	if _, err := base64.StdEncoding.DecodeString(body); err != nil {
		t.Errorf("post-prefix body should be base64; got %q (decode err %v)", body, err)
	}
}
