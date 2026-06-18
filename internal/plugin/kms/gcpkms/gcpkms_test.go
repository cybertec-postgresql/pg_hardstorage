package gcpkms_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"

	stdkms "github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/kms/gcpkms"
)

// fakeGCPKMS implements gcpkms.Client without contacting GCP.
type fakeGCPKMS struct {
	encryptName   string
	encryptCipher []byte
	decryptCipher []byte
	decryptPlain  []byte
	decryptErr    error
	destroyName   string
	destroyErr    error
	getName       string
	getResp       *kmspb.CryptoKey
	closed        bool
}

func (f *fakeGCPKMS) Encrypt(_ context.Context, in *kmspb.EncryptRequest, _ ...any) (*kmspb.EncryptResponse, error) {
	f.encryptName = in.Name
	// Simple round-trip: prepend "ENC:" so Decrypt can verify.
	return &kmspb.EncryptResponse{
		Name:       in.Name,
		Ciphertext: append([]byte("ENC:"), in.Plaintext...),
	}, nil
}
func (f *fakeGCPKMS) Decrypt(_ context.Context, in *kmspb.DecryptRequest, _ ...any) (*kmspb.DecryptResponse, error) {
	f.decryptCipher = in.Ciphertext
	if f.decryptErr != nil {
		return nil, f.decryptErr
	}
	if !strings.HasPrefix(string(in.Ciphertext), "ENC:") {
		return nil, errors.New("decrypt: not our ciphertext")
	}
	return &kmspb.DecryptResponse{Plaintext: in.Ciphertext[len("ENC:"):]}, nil
}
func (f *fakeGCPKMS) DestroyCryptoKeyVersion(_ context.Context, in *kmspb.DestroyCryptoKeyVersionRequest, _ ...any) (*kmspb.CryptoKeyVersion, error) {
	f.destroyName = in.Name
	if f.destroyErr != nil {
		return nil, f.destroyErr
	}
	return &kmspb.CryptoKeyVersion{Name: in.Name, State: kmspb.CryptoKeyVersion_DESTROY_SCHEDULED}, nil
}
func (f *fakeGCPKMS) GetCryptoKey(_ context.Context, in *kmspb.GetCryptoKeyRequest, _ ...any) (*kmspb.CryptoKey, error) {
	f.getName = in.Name
	if f.getResp != nil {
		return f.getResp, nil
	}
	return &kmspb.CryptoKey{
		Name:    in.Name,
		Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT,
		Primary: &kmspb.CryptoKeyVersion{
			Name:            in.Name + "/cryptoKeyVersions/3",
			State:           kmspb.CryptoKeyVersion_ENABLED,
			ProtectionLevel: kmspb.ProtectionLevel_HSM,
		},
	}, nil
}
func (f *fakeGCPKMS) Close() error { f.closed = true; return nil }

const sampleKeyRef = "gcp-kms://projects/my-proj/locations/global/keyRings/my-ring/cryptoKeys/my-key"

func TestProvider_WrapUnwrapRoundTrip(t *testing.T) {
	cli := &fakeGCPKMS{}
	p, err := gcpkms.NewWithClient(sampleKeyRef, cli)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	dek := []byte("32-byte-aes-key-padded-here-OK")
	wrapped, err := p.WrapDEK(context.Background(), dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if cli.encryptName != "projects/my-proj/locations/global/keyRings/my-ring/cryptoKeys/my-key" {
		t.Errorf("Encrypt key name = %q", cli.encryptName)
	}
	got, err := p.UnwrapDEK(context.Background(), wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if string(got) != string(dek) {
		t.Errorf("round-trip lost bytes: %q", got)
	}
}

func TestProvider_VersionPinnedKEKRef(t *testing.T) {
	cli := &fakeGCPKMS{}
	ref := sampleKeyRef + "/cryptoKeyVersions/3"
	p, err := gcpkms.NewWithClient(ref, cli)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	if p.KEKRef() != ref {
		t.Errorf("KEKRef = %q", p.KEKRef())
	}
	dek := []byte("dek")
	if _, err := p.WrapDEK(context.Background(), dek); err != nil {
		t.Fatal(err)
	}
	// With a version-pinned ref, Encrypt should target the
	// versioned path.
	if !strings.HasSuffix(cli.encryptName, "/cryptoKeyVersions/3") {
		t.Errorf("Encrypt should target versioned path; got %q", cli.encryptName)
	}
}

func TestProvider_UnwrapErrorWrapsSentinel(t *testing.T) {
	cli := &fakeGCPKMS{decryptErr: errors.New("PermissionDenied")}
	p, _ := gcpkms.NewWithClient(sampleKeyRef, cli)
	_, err := p.UnwrapDEK(context.Background(), []byte("anything"))
	if !errors.Is(err, stdkms.ErrUnwrap) {
		t.Errorf("expected ErrUnwrap wrapping; got %v", err)
	}
}

func TestProvider_Shred_RequiresVersion(t *testing.T) {
	cli := &fakeGCPKMS{}
	p, _ := gcpkms.NewWithClient(sampleKeyRef, cli)
	err := p.Shred(context.Background())
	if err == nil {
		t.Fatal("Shred without /cryptoKeyVersions/ should refuse")
	}
	if !errors.Is(err, stdkms.ErrShredFailed) {
		t.Errorf("expected ErrShredFailed; got %v", err)
	}
	if !strings.Contains(err.Error(), "version-pinned") {
		t.Errorf("error should explain version requirement: %v", err)
	}
}

func TestProvider_Shred_VersionPinned(t *testing.T) {
	cli := &fakeGCPKMS{}
	ref := sampleKeyRef + "/cryptoKeyVersions/3"
	p, _ := gcpkms.NewWithClient(ref, cli)
	if err := p.Shred(context.Background()); err != nil {
		t.Fatalf("Shred: %v", err)
	}
	if !strings.HasSuffix(cli.destroyName, "/cryptoKeyVersions/3") {
		t.Errorf("Shred destroy name = %q", cli.destroyName)
	}
}

func TestProvider_ShredErrorWrapsSentinel(t *testing.T) {
	cli := &fakeGCPKMS{destroyErr: errors.New("KeyDestroyed")}
	ref := sampleKeyRef + "/cryptoKeyVersions/1"
	p, _ := gcpkms.NewWithClient(ref, cli)
	err := p.Shred(context.Background())
	if !errors.Is(err, stdkms.ErrShredFailed) {
		t.Errorf("expected ErrShredFailed; got %v", err)
	}
}

func TestProvider_DescribeKey(t *testing.T) {
	cli := &fakeGCPKMS{}
	p, _ := gcpkms.NewWithClient(sampleKeyRef, cli)
	body, err := p.DescribeKey(context.Background())
	if err != nil {
		t.Fatalf("DescribeKey: %v", err)
	}
	if body["purpose"] != "ENCRYPT_DECRYPT" {
		t.Errorf("purpose = %v", body["purpose"])
	}
	if body["primary_protection_level"] != "HSM" {
		t.Errorf("primary_protection_level = %v", body["primary_protection_level"])
	}
}

func TestProvider_RejectsBadKEKRef(t *testing.T) {
	cli := &fakeGCPKMS{}
	cases := []string{
		"local:default",              // wrong scheme
		"gcp-kms://",                 // empty
		"gcp-kms://just-some-string", // not a resource path
		"gcp-kms://projects/p",       // incomplete
		"gcp-kms://projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/", // empty version
	}
	for _, kr := range cases {
		_, err := gcpkms.NewWithClient(kr, cli)
		if err == nil {
			t.Errorf("expected error for %q", kr)
		}
	}
}

func TestProvider_FIPSMode(t *testing.T) {
	cli := &fakeGCPKMS{}
	p, _ := gcpkms.NewWithClient(sampleKeyRef, cli, gcpkms.WithFIPSMode(true))
	if !p.FIPSMode() {
		t.Error("FIPSMode option not honoured")
	}
}

func TestProvider_ClosedRefuses(t *testing.T) {
	cli := &fakeGCPKMS{}
	p, _ := gcpkms.NewWithClient(sampleKeyRef, cli)
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
		if s == gcpkms.Scheme {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("gcp-kms not registered; schemes=%v", schemes)
	}
}
