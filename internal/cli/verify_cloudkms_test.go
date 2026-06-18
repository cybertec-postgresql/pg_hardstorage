package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

type cloudVerifyProvider struct{ ref string }

func (p *cloudVerifyProvider) Name() string                                          { return "fake-verify-kms" }
func (p *cloudVerifyProvider) KEKRef() string                                        { return p.ref }
func (p *cloudVerifyProvider) WrapDEK(_ context.Context, dek []byte) ([]byte, error) { return dek, nil }
func (p *cloudVerifyProvider) UnwrapDEK(_ context.Context, w []byte) ([]byte, error) { return w, nil }
func (p *cloudVerifyProvider) Shred(_ context.Context) error                         { return nil }
func (p *cloudVerifyProvider) FIPSMode() bool                                        { return false }
func (p *cloudVerifyProvider) Close() error                                          { return nil }

// TestBuildVerifyCAS_CloudKMS_RoundTrip pins issue #102 for the verify glue:
// a cloud-KMS-encrypted backup is verifiable — the DEK is unwrapped
// server-side through the registered provider.
func TestBuildVerifyCAS_CloudKMS_RoundTrip(t *testing.T) {
	kms.DefaultRegistry.Register("fake-verify-kms", func(_ context.Context, ref string, _ map[string]any) (kms.Provider, error) {
		return &cloudVerifyProvider{ref: ref}, nil
	})
	t.Cleanup(func() {
		kms.DefaultRegistry.Register("fake-verify-kms", func(_ context.Context, _ string, _ map[string]any) (kms.Provider, error) {
			return nil, errors.New("cleared")
		})
	})

	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	dek, _ := encryption.GenerateDEK()
	m := &backup.Manifest{Encryption: &backup.EncryptionInfo{
		Scheme:     "aes-256-gcm",
		KEKRef:     "fake-verify-kms://key",
		WrappedDEK: base64.StdEncoding.EncodeToString(dek[:]),
	}}

	enc, _ := aesgcm.New(dek[:])
	writeCAS := casdefault.NewEncrypted(sp, enc)
	body := []byte("verify cloud secret")
	ci, err := writeCAS.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}

	cas, err := buildVerifyCAS(context.Background(), sp, m, nil)
	if err != nil {
		t.Fatalf("buildVerifyCAS (cloud): %v", err)
	}
	got, err := cas.GetChunkBytes(context.Background(), ci.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("verify cloud round-trip differs: got %q, want %q", got, body)
	}
}
