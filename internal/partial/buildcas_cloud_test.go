package partial

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// TestBuildCAS_CloudKMS_RoundTrip pins issue #102 for the partial-restore
// glue: a backup wrapped with a cloud KMS KEK is decryptable — the DEK is
// unwrapped server-side via unwrapDEK, and the local kekForRef is NOT
// consulted for a cloud KEKRef.
func TestBuildCAS_CloudKMS_RoundTrip(t *testing.T) {
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	dek, _ := encryption.GenerateDEK()
	m := &backup.Manifest{Encryption: &backup.EncryptionInfo{
		Scheme:     "aes-256-gcm",
		KEKRef:     "aws-kms://arn:aws:kms:eu-central-1:123:key/abc",
		WrappedDEK: base64.StdEncoding.EncodeToString(dek[:]),
	}}

	enc, _ := aesgcm.New(dek[:])
	writeCAS := casdefault.NewEncrypted(sp, enc)
	body := []byte("partial cloud secret")
	ci, err := writeCAS.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}

	cas, err := buildCAS(context.Background(), sp, m,
		func(string) ([encryption.KeyLen]byte, error) {
			t.Error("local kekForRef must not be called for a cloud KEKRef")
			return [encryption.KeyLen]byte{}, errors.New("should not be called")
		},
		func(_ context.Context, _ string, w []byte) ([]byte, error) { return w, nil })
	if err != nil {
		t.Fatalf("buildCAS (cloud): %v", err)
	}
	got, err := cas.GetChunkBytes(context.Background(), ci.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("cloud round-trip differs: got %q, want %q", got, body)
	}

	// A cloud-KMS backup with no unwrap resolver is rejected (not silently
	// routed to the local path).
	if _, err := buildCAS(context.Background(), sp, m, nil, nil); err == nil {
		t.Error("expected error for cloud KEKRef without unwrapDEK")
	}
}
