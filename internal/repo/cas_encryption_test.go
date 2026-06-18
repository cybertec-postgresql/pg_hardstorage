package repo_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func newSP(t *testing.T) storage.StoragePlugin {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

func mustEncryptor(t *testing.T) encryption.Encryptor {
	t.Helper()
	key := make([]byte, encryption.KeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	enc, err := aesgcm.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func TestCAS_Encrypted_RoundTripPlaintextRecovered(t *testing.T) {
	sp := newSP(t)
	enc := mustEncryptor(t)
	cas := repo.NewCAS(sp, repo.WithEncryptor(enc))

	body := []byte("the quick brown fox jumps over the lazy dog")
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := cas.GetChunkBytes(context.Background(), info.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("plaintext round-trip differs; got %q", got)
	}
}

func TestCAS_Encrypted_OnDiskBytesAreNotPlaintext(t *testing.T) {
	sp := newSP(t)
	enc := mustEncryptor(t)
	cas := repo.NewCAS(sp, repo.WithEncryptor(enc))

	body := bytes.Repeat([]byte("THIS_IS_PLAINTEXT_LOOK_FOR_ME"), 64)
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := sp.Get(context.Background(), repo.ChunkKey(info.Hash))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var disk bytes.Buffer
	_, _ = disk.ReadFrom(rc)
	if bytes.Contains(disk.Bytes(), []byte("THIS_IS_PLAINTEXT_LOOK_FOR_ME")) {
		t.Errorf("plaintext marker present on disk — encryption did not apply\n%x", disk.Bytes())
	}
	// On-disk envelope should have v0x02 + non-zero EncryptionAlgo.
	if disk.Len() < 15 {
		t.Fatalf("on-disk too short for v0x02 envelope: %d bytes", disk.Len())
	}
	algo, encFields, _, err := compression.ReadEnvelope(disk.Bytes())
	if err != nil {
		t.Fatalf("envelope decode: %v", err)
	}
	if !encFields.IsEncrypted() {
		t.Errorf("envelope reports unencrypted; want encrypted. fields=%+v", encFields)
	}
	if encFields.EncryptionAlgo != byte(encryption.AlgoAESGCM) {
		t.Errorf("EncryptionAlgo = %d, want %d (AES-GCM)", encFields.EncryptionAlgo, encryption.AlgoAESGCM)
	}
	_ = algo
}

func TestCAS_Encrypted_DifferentKey_CannotDecrypt(t *testing.T) {
	sp := newSP(t)
	encA := mustEncryptor(t)
	encB := mustEncryptor(t)

	casA := repo.NewCAS(sp, repo.WithEncryptor(encA))
	body := []byte("only A wrote this")
	info, err := casA.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}

	casB := repo.NewCAS(sp, repo.WithEncryptor(encB))
	_, err = casB.GetChunkBytes(context.Background(), info.Hash)
	if err == nil {
		t.Fatal("decrypt with foreign key should fail")
	}
	if !errors.Is(err, encryption.ErrAuthenticationFailed) {
		t.Errorf("expected ErrAuthenticationFailed; got %v", err)
	}
}

func TestCAS_Encrypted_NoKey_CannotDecrypt(t *testing.T) {
	// Write encrypted, then try to read with a CAS that has no
	// encryptor at all. Should fail at registry-lookup time with
	// ErrUnknownAlgorithm.
	sp := newSP(t)
	enc := mustEncryptor(t)
	casWriter := repo.NewCAS(sp, repo.WithEncryptor(enc))
	info, err := casWriter.PutChunk(context.Background(), []byte("classified"))
	if err != nil {
		t.Fatal(err)
	}

	casReader := repo.NewCAS(sp) // no encryptor
	_, err = casReader.GetChunkBytes(context.Background(), info.Hash)
	if err == nil {
		t.Fatal("decrypt without key should fail")
	}
	if !errors.Is(err, encryption.ErrUnknownAlgorithm) {
		t.Errorf("expected ErrUnknownAlgorithm; got %v", err)
	}
}

func TestCAS_Mixed_EncryptedAndUnencryptedCoexist(t *testing.T) {
	// One repo, two CAS instances: one with encryption, one without.
	// They write different chunks (different plaintext) and both
	// round-trip independently.
	sp := newSP(t)
	enc := mustEncryptor(t)

	casPlain := repo.NewCAS(sp)
	casCrypt := repo.NewCAS(sp, repo.WithEncryptor(enc))

	plain := []byte("plain chunk body")
	cipher := []byte("encrypted chunk body")

	infoP, err := casPlain.PutChunk(context.Background(), plain)
	if err != nil {
		t.Fatal(err)
	}
	infoC, err := casCrypt.PutChunk(context.Background(), cipher)
	if err != nil {
		t.Fatal(err)
	}

	gotP, err := casPlain.GetChunkBytes(context.Background(), infoP.Hash)
	if err != nil {
		t.Errorf("plain CAS plain chunk: %v", err)
	}
	if !bytes.Equal(gotP, plain) {
		t.Error("plain round-trip differs")
	}

	gotC, err := casCrypt.GetChunkBytes(context.Background(), infoC.Hash)
	if err != nil {
		t.Errorf("encrypted CAS encrypted chunk: %v", err)
	}
	if !bytes.Equal(gotC, cipher) {
		t.Error("encrypted round-trip differs")
	}

	// And — the encrypted CAS should be able to read the unencrypted
	// chunk too (envelope v0x01 / EncryptionAlgo=0 → no decrypt step).
	gotPviaCrypt, err := casCrypt.GetChunkBytes(context.Background(), infoP.Hash)
	if err != nil {
		t.Errorf("encrypted CAS reading unencrypted chunk: %v", err)
	}
	if !bytes.Equal(gotPviaCrypt, plain) {
		t.Error("encrypted CAS produced wrong plaintext for an unencrypted chunk")
	}
}

func TestCAS_Encrypted_DedupsByPlaintextHash(t *testing.T) {
	// Two encrypts of the same plaintext under the same key should
	// dedup at the CAS level — the chunk key is plaintext SHA-256,
	// not ciphertext SHA-256.
	sp := newSP(t)
	enc := mustEncryptor(t)
	cas := repo.NewCAS(sp, repo.WithEncryptor(enc))

	body := []byte("same plaintext twice")
	info1, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	info2, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if info1.Hash != info2.Hash {
		t.Errorf("hashes differ for same plaintext: %x vs %x", info1.Hash, info2.Hash)
	}
	if !info2.Deduped {
		t.Error("second Put of same plaintext should report Deduped=true")
	}
}
