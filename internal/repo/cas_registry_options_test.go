package repo_test

// The CAS registry options had no test executing them (coverage
// ratchet). WithRegistry and WithEncryptionRegistry decide, per CAS
// instance, which codecs can DECODE stored chunks and which decryptors
// can open them — so a mis-wired option is silent at write time and
// only surfaces when someone tries to read the data back. These tests
// pin the property that matters: what a CAS writes, that same CAS (and
// only a correctly-configured one) can read.

import (
	"bytes"
	"context"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression/zstd"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func optSP(t *testing.T) storage.StoragePlugin {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

func newTestEncryptor(t *testing.T) encryption.Encryptor {
	t.Helper()
	key := bytes.Repeat([]byte{0x2b}, 32)
	e, err := aesgcm.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// WithEncryptor must make the round trip work AND the bytes on disk
// must actually be encrypted — a no-op encryptor that "succeeds"
// would pass a naive round-trip test while leaving plaintext at rest.
func TestWithEncryptor_RoundTripsAndStoresCiphertext(t *testing.T) {
	sp := optSP(t)
	ctx := context.Background()
	plaintext := []byte("row data that must not appear on disk verbatim")

	cas := repo.NewCAS(sp, repo.WithEncryptor(newTestEncryptor(t)))
	info, err := cas.PutChunk(ctx, plaintext)
	if err != nil {
		t.Fatalf("PutChunk: %v", err)
	}
	got, err := cas.GetChunkBytes(ctx, info.Hash)
	if err != nil {
		t.Fatalf("GetChunkBytes: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round trip lost bytes: %q", got)
	}

	// The stored object must NOT contain the plaintext.
	rc, err := sp.Get(ctx, repo.ChunkKey(info.Hash))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	stored := new(bytes.Buffer)
	if _, err := stored.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.Bytes(), plaintext) {
		t.Error("plaintext found in the stored chunk — WithEncryptor did not encrypt at rest")
	}
}

// A CAS with NO decryptor registered must refuse the chunk rather than
// return garbage: silently handing back ciphertext-as-plaintext would
// corrupt a restore.
func TestEncryptedChunk_UnreadableWithoutDecryptor(t *testing.T) {
	sp := optSP(t)
	ctx := context.Background()

	writer := repo.NewCAS(sp, repo.WithEncryptor(newTestEncryptor(t)))
	info, err := writer.PutChunk(ctx, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}

	// Fresh CAS over the same storage, no encryptor installed.
	reader := repo.NewCAS(sp)
	if got, err := reader.GetChunkBytes(ctx, info.Hash); err == nil {
		t.Fatalf("a CAS with no decryptor returned %q instead of refusing", got)
	}
}

// WithEncryptionRegistry replaces the registry wholesale. Installing an
// EMPTY one must therefore break reads — this pins that the option is
// actually wired to the read path (a no-op option would let the read
// succeed and hide the misconfiguration).
func TestWithEncryptionRegistry_ReplacesTheDecryptorSet(t *testing.T) {
	sp := optSP(t)
	ctx := context.Background()
	enc := newTestEncryptor(t)

	writer := repo.NewCAS(sp, repo.WithEncryptor(enc))
	info, err := writer.PutChunk(ctx, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}

	// Order matters: the empty registry lands AFTER WithEncryptor, so
	// it discards the decryptor that option registered.
	blind := repo.NewCAS(sp,
		repo.WithEncryptor(enc),
		repo.WithEncryptionRegistry(encryption.NewRegistry()),
	)
	if _, err := blind.GetChunkBytes(ctx, info.Hash); err == nil {
		t.Error("WithEncryptionRegistry did not replace the decryptor set — the read " +
			"succeeded with an empty registry, so the option is not wired to the read path")
	}
}

// WithRegistry controls which COMPRESSION codecs a CAS can decode.
//
// Isolating the option needs care: the DEFAULT registry knows only
// AlgoNone, so a zstd chunk fails to read with or without an empty
// WithRegistry — an "empty registry fails" assertion would pass for
// the wrong reason and would not catch a no-op WithRegistry (verified:
// stubbing the option to a no-op still passed that shape). The
// discriminating test is the positive direction: a reader whose
// injected registry KNOWS zstd must succeed where the default reader
// fails. That can only pass if WithRegistry actually reaches the read
// path.
func TestWithRegistry_InjectedCodecEnablesTheRead(t *testing.T) {
	sp := optSP(t)
	ctx := context.Background()

	writer := repo.NewCAS(sp, repo.WithCompressor(zstd.NewDefault()))
	body := bytes.Repeat([]byte("compressible-"), 512)
	info, err := writer.PutChunk(ctx, body)
	if err != nil {
		t.Fatalf("PutChunk: %v", err)
	}

	// Baseline: the default registry (AlgoNone only) cannot decode it.
	if _, err := repo.NewCAS(sp).GetChunkBytes(ctx, info.Hash); err == nil {
		t.Fatal("fixture invalid: the default reader decoded a zstd chunk, so this test " +
			"cannot show that WithRegistry made the difference")
	}

	// A registry that knows zstd, injected via the option, must read it.
	reg := compression.NewRegistry()
	reg.Register(compression.AlgoZstd, zstd.NewDefault())
	reader := repo.NewCAS(sp, repo.WithRegistry(reg))
	got, err := reader.GetChunkBytes(ctx, info.Hash)
	if err != nil {
		t.Fatalf("WithRegistry did not reach the read path — injected zstd codec unused: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Error("chunk decoded through the injected registry lost bytes")
	}
}
