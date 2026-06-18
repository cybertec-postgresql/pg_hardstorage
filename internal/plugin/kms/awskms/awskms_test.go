package awskms_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"

	stdkms "github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/kms/awskms"
)

// fakeKMS implements awskms.Client.  Each method records its
// last call so tests can assert the SDK was driven correctly.
type fakeKMS struct {
	encryptKeyID      string
	encryptCiphertext []byte
	decryptCiphertext []byte
	decryptPlaintext  []byte
	decryptErr        error
	scheduleKeyID     string
	scheduleWindow    int32
	scheduleErr       error
	describeKeyID     string
	describeOut       *kms.DescribeKeyOutput
}

func (f *fakeKMS) Encrypt(_ context.Context, in *kms.EncryptInput, _ ...func(*kms.Options)) (*kms.EncryptOutput, error) {
	f.encryptKeyID = aws.ToString(in.KeyId)
	return &kms.EncryptOutput{CiphertextBlob: append([]byte("WRAP:"), in.Plaintext...)}, nil
}
func (f *fakeKMS) Decrypt(_ context.Context, in *kms.DecryptInput, _ ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	f.decryptCiphertext = in.CiphertextBlob
	if f.decryptErr != nil {
		return nil, f.decryptErr
	}
	if !strings.HasPrefix(string(in.CiphertextBlob), "WRAP:") {
		return nil, errors.New("decrypt: not our ciphertext")
	}
	pt := in.CiphertextBlob[len("WRAP:"):]
	return &kms.DecryptOutput{Plaintext: pt}, nil
}
func (f *fakeKMS) ScheduleKeyDeletion(_ context.Context, in *kms.ScheduleKeyDeletionInput, _ ...func(*kms.Options)) (*kms.ScheduleKeyDeletionOutput, error) {
	f.scheduleKeyID = aws.ToString(in.KeyId)
	f.scheduleWindow = aws.ToInt32(in.PendingWindowInDays)
	if f.scheduleErr != nil {
		return nil, f.scheduleErr
	}
	return &kms.ScheduleKeyDeletionOutput{}, nil
}
func (f *fakeKMS) DescribeKey(_ context.Context, in *kms.DescribeKeyInput, _ ...func(*kms.Options)) (*kms.DescribeKeyOutput, error) {
	f.describeKeyID = aws.ToString(in.KeyId)
	if f.describeOut == nil {
		return &kms.DescribeKeyOutput{
			KeyMetadata: &kmstypes.KeyMetadata{
				KeyId:      aws.String("abcd"),
				Arn:        aws.String("arn:aws:kms:us-east-1:123:key/abcd"),
				Enabled:    true,
				KeyState:   kmstypes.KeyStateEnabled,
				KeyUsage:   kmstypes.KeyUsageTypeEncryptDecrypt,
				KeyManager: kmstypes.KeyManagerTypeCustomer,
			},
		}, nil
	}
	return f.describeOut, nil
}

func TestProvider_WrapUnwrapRoundTrip(t *testing.T) {
	cli := &fakeKMS{}
	p, err := awskms.NewWithClient("aws-kms://arn:aws:kms:us-east-1:123:key/abcd", cli)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	dek := []byte("32-byte-aes-key-padded-here-OK")
	wrapped, err := p.WrapDEK(context.Background(), dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if cli.encryptKeyID != "arn:aws:kms:us-east-1:123:key/abcd" {
		t.Errorf("Encrypt key id = %q", cli.encryptKeyID)
	}
	got, err := p.UnwrapDEK(context.Background(), wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if string(got) != string(dek) {
		t.Errorf("round-trip lost bytes: %q", got)
	}
}

func TestProvider_UnwrapErrorWrapsSentinel(t *testing.T) {
	cli := &fakeKMS{decryptErr: errors.New("AccessDenied")}
	p, _ := awskms.NewWithClient("aws-kms://abcd", cli)
	_, err := p.UnwrapDEK(context.Background(), []byte("anything"))
	if !errors.Is(err, stdkms.ErrUnwrap) {
		t.Errorf("expected ErrUnwrap wrapping, got %v", err)
	}
}

func TestProvider_Shred(t *testing.T) {
	cli := &fakeKMS{}
	p, _ := awskms.NewWithClient("aws-kms://my-key", cli, awskms.WithPendingWindow(7))
	if err := p.Shred(context.Background()); err != nil {
		t.Fatalf("Shred: %v", err)
	}
	if cli.scheduleKeyID != "my-key" {
		t.Errorf("Shred KeyId = %q", cli.scheduleKeyID)
	}
	if cli.scheduleWindow != 7 {
		t.Errorf("Shred pending window = %d, want 7", cli.scheduleWindow)
	}
}

func TestProvider_ShredErrorWrapsSentinel(t *testing.T) {
	cli := &fakeKMS{scheduleErr: errors.New("KeyUnavailableException")}
	p, _ := awskms.NewWithClient("aws-kms://my-key", cli)
	err := p.Shred(context.Background())
	if !errors.Is(err, stdkms.ErrShredFailed) {
		t.Errorf("expected ErrShredFailed, got %v", err)
	}
}

func TestProvider_DescribeKey(t *testing.T) {
	cli := &fakeKMS{}
	p, _ := awskms.NewWithClient("aws-kms://abcd", cli)
	body, err := p.DescribeKey(context.Background())
	if err != nil {
		t.Fatalf("DescribeKey: %v", err)
	}
	if body["enabled"] != true {
		t.Errorf("enabled = %v", body["enabled"])
	}
	if body["key_manager"] != "customer" {
		t.Errorf("key_manager = %v", body["key_manager"])
	}
	if body["state"] != "Enabled" {
		t.Errorf("state = %v", body["state"])
	}
}

func TestProvider_RejectsBadKEKRef(t *testing.T) {
	cli := &fakeKMS{}
	cases := []string{
		"local:default", // wrong scheme
		"aws-kms://",    // empty id
		"aws-kms",       // missing scheme://
	}
	for _, kr := range cases {
		_, err := awskms.NewWithClient(kr, cli)
		if err == nil {
			t.Errorf("expected error for %q", kr)
		}
	}
}

func TestProvider_FIPSMode(t *testing.T) {
	cli := &fakeKMS{}
	p, _ := awskms.NewWithClient("aws-kms://x", cli, awskms.WithFIPSMode(true))
	if !p.FIPSMode() {
		t.Error("FIPSMode option not honoured")
	}
}

func TestProvider_ClosedRefuses(t *testing.T) {
	cli := &fakeKMS{}
	p, _ := awskms.NewWithClient("aws-kms://x", cli)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.WrapDEK(context.Background(), []byte("x")); err == nil {
		t.Error("WrapDEK on closed provider should fail")
	}
}

func TestProvider_RegistryRoundTrip(t *testing.T) {
	// Provider registers itself in init().  The registry's
	// builder requires real AWS config (or LocalStack); a unit
	// test can't drive Open() end-to-end.  Instead we just
	// confirm the scheme is in the registry's claim list.
	schemes := stdkms.DefaultRegistry.Schemes()
	found := false
	for _, s := range schemes {
		if s == awskms.Scheme {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("aws-kms not registered; schemes=%v", schemes)
	}
}
