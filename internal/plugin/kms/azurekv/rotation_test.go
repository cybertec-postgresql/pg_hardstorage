package azurekv_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/kms/azurekv"
)

// rotatingVault models a Key Vault key with several versions. A wrap
// or unwrap with version "" uses the CURRENT version, as Azure does;
// ciphertext is only openable under the version that produced it.
type rotatingVault struct {
	fakeAzureKV
	current       string
	lastUnwrapVer string
}

func (r *rotatingVault) wrapAs(ver string, dek []byte) []byte {
	return append([]byte("K"+ver+":"), dek...)
}

func (r *rotatingVault) Wrap(_ context.Context, version, _ string, dek []byte) ([]byte, error) {
	if version == "" {
		version = r.current
	}
	return r.wrapAs(version, dek), nil
}

func (r *rotatingVault) WrapVersioned(ctx context.Context, version, alg string, dek []byte) ([]byte, string, error) {
	if version == "" {
		version = r.current
	}
	ct, err := r.Wrap(ctx, version, alg, dek)
	return ct, version, err
}

func (r *rotatingVault) Unwrap(_ context.Context, version, _ string, ct []byte) ([]byte, error) {
	if version == "" {
		version = r.current
	}
	r.lastUnwrapVer = version
	prefix := []byte("K" + version + ":")
	if !bytes.HasPrefix(ct, prefix) {
		return nil, errors.New("BadParameter: the ciphertext was not produced by this key version")
	}
	return ct[len(prefix):], nil
}

var dek32 = []byte("0123456789abcdef0123456789abcdef")

// TestUnwrapDEK_AfterAzureSideRotation pins H15. With an unversioned
// KEKRef the DEK is wrapped under whatever version is current, but the
// version was thrown away and UnwrapDEK always asked for "latest". The
// first rotation in the Azure portal made every older backup's DEK
// unwrappable.
func TestUnwrapDEK_AfterAzureSideRotation(t *testing.T) {
	v := &rotatingVault{current: "v1"}
	p, err := azurekv.NewWithClient(sampleKeyRef, v)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := p.WrapDEK(context.Background(), dek32)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}

	v.current = "v2" // az keyvault key rotate

	got, err := p.UnwrapDEK(context.Background(), wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK after rotation: %v (every pre-rotation backup is unrestorable)", err)
	}
	if !bytes.Equal(got, dek32) {
		t.Fatal("wrong DEK")
	}
	if v.lastUnwrapVer != "v1" {
		t.Errorf("unwrapped with version %q, want the wrapping version v1", v.lastUnwrapVer)
	}
}

// TestUnwrapDEK_LegacyBlobStillUnwraps: blobs written before the
// version was recorded carry no version and keep using the KEKRef's
// (latest, when unversioned) — exactly the old behaviour.
func TestUnwrapDEK_LegacyBlobStillUnwraps(t *testing.T) {
	v := &rotatingVault{current: "v1"}
	p, err := azurekv.NewWithClient(sampleKeyRef, v)
	if err != nil {
		t.Fatal(err)
	}
	legacy := v.wrapAs("v1", dek32)
	got, err := p.UnwrapDEK(context.Background(), legacy)
	if err != nil {
		t.Fatalf("legacy blob: %v", err)
	}
	if !bytes.Equal(got, dek32) {
		t.Fatal("wrong DEK")
	}
	if v.lastUnwrapVer != "v1" {
		t.Errorf("legacy unwrap used %q, want latest (v1)", v.lastUnwrapVer)
	}
}

// TestWrapDEK_PinnedKEKRefStaysRaw: a version-pinned KEKRef already
// records the version, so the blob stays in the legacy raw form.
func TestWrapDEK_PinnedKEKRefStaysRaw(t *testing.T) {
	v := &rotatingVault{current: "v9"}
	p, err := azurekv.NewWithClient(sampleKeyRef+"/v3", v)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := p.WrapDEK(context.Background(), dek32)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(wrapped), "Kv3:") {
		t.Errorf("pinned wrap = %q, want the raw vault ciphertext", wrapped[:8])
	}
}
