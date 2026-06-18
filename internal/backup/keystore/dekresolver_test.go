package keystore_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
)

// TestDEKResolver_LocalRoundTrip pins the shared DEK resolver used to wire
// restore.Options.UnwrapDEK across the restore-engine callers (issue #102).
// The local path unwraps the DEK with the keyring's KEK; the cloud path is
// exercised by keystore.UnwrapDEK's own tests (DEKResolver just forwards).
func TestDEKResolver_LocalRoundTrip(t *testing.T) {
	dir := t.TempDir()
	kek, _, err := keystore.LoadOrGenerateKEK(dir)
	if err != nil {
		t.Fatal(err)
	}
	dek := make([]byte, encryption.KeyLen)
	rand.Read(dek)

	// Wrap the DEK in the on-disk shape (`[12 nonce | ct+tag]`).
	block, _ := aes.NewCipher(kek[:])
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, 12)
	rand.Read(nonce)
	wrapped := append(append([]byte{}, nonce...), gcm.Seal(nil, nonce, dek, nil)...)

	resolve := keystore.DEKResolver(dir, nil)
	got, err := resolve(context.Background(), keystore.KEKRefLocal, wrapped)
	if err != nil {
		t.Fatalf("DEKResolver local: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Error("local DEK round-trip lost bytes")
	}
}
