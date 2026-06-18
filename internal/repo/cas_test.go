package repo_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func newCAS(t *testing.T) *repo.CAS {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return repo.NewCAS(sp)
}

func TestPutGet_RoundTrip(t *testing.T) {
	c := newCAS(t)
	body := []byte("the quick brown fox")
	info, err := c.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if info.Size != int64(len(body)) {
		t.Errorf("size = %d", info.Size)
	}
	want := sha256.Sum256(body)
	if info.Hash != want {
		t.Errorf("hash mismatch: got %x", info.Hash)
	}
	if info.Deduped {
		t.Error("first put should not be deduped")
	}

	got, err := c.GetChunkBytes(context.Background(), info.Hash)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("get round-trip: got %q want %q", got, body)
	}
}

func TestPut_DedupOnSecondCall(t *testing.T) {
	c := newCAS(t)
	body := []byte("hello world")

	info1, err := c.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if info1.Deduped {
		t.Error("first put should not be deduped")
	}

	info2, err := c.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !info2.Deduped {
		t.Error("second put with same content must report deduped")
	}
	if info2.Hash != info1.Hash || info2.Size != info1.Size {
		t.Errorf("deduped put returned different metadata: %+v vs %+v", info2, info1)
	}
}

func TestHasChunk(t *testing.T) {
	c := newCAS(t)
	body := []byte("present")
	hash := sha256.Sum256(body)

	has, err := c.HasChunk(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("HasChunk before put should be false")
	}

	if _, err := c.PutChunk(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	has, err = c.HasChunk(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("HasChunk after put should be true")
	}
}

func TestDelete_Idempotent(t *testing.T) {
	c := newCAS(t)
	body := []byte("delete me")
	info, err := c.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteChunk(context.Background(), info.Hash); err != nil {
		t.Fatal(err)
	}
	// Second delete should not error; cache must reflect absence.
	if err := c.DeleteChunk(context.Background(), info.Hash); err != nil {
		t.Errorf("second delete should be a no-op: %v", err)
	}
	has, err := c.HasChunk(context.Background(), info.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("HasChunk after delete should be false")
	}
}

func TestDifferentBodiesDifferentKeys(t *testing.T) {
	c := newCAS(t)
	a, _ := c.PutChunk(context.Background(), []byte("foo"))
	b, _ := c.PutChunk(context.Background(), []byte("bar"))
	if a.Hash == b.Hash {
		t.Error("different bodies should hash differently")
	}
	if repo.ChunkKey(a.Hash) == repo.ChunkKey(b.Hash) {
		t.Error("different hashes should produce different keys")
	}
}

func TestChunkKey_Format(t *testing.T) {
	hash := sha256.Sum256([]byte("anything"))
	key := repo.ChunkKey(hash)
	hexHash := hex.EncodeToString(hash[:])
	want := "chunks/sha256/" + hexHash[0:2] + "/" + hexHash[2:4] + "/" + hexHash + ".chk"
	if key != want {
		t.Errorf("ChunkKey = %q, want %q", key, want)
	}
}

func TestParseChunkKey_RoundTrip(t *testing.T) {
	hash := sha256.Sum256([]byte("hello"))
	key := repo.ChunkKey(hash)
	got, err := repo.ParseChunkKey(key)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != hash {
		t.Errorf("hash round-trip failed")
	}
}

func TestParseChunkKey_RejectsBogus(t *testing.T) {
	for _, bad := range []string{
		"",
		"foo/bar/baz",
		"chunks/sha256/aa/bb/aabb.chk", // too short
		"chunks/sha256/aa/bb/cccc1111111111111111111111111111111111111111111111111111111111111111.chk", // prefix mismatch
		"chunks/sha256/aa/bb/aabb1111111111111111111111111111111111111111111111111111111111111111.txt", // wrong suffix
		"chunks/sha256/aa//aabb1111111111111111111111111111111111111111111111111111111111111111.chk",   // missing dir
	} {
		_, err := repo.ParseChunkKey(bad)
		if !errors.Is(err, repo.ErrNotAChunkKey) {
			t.Errorf("ParseChunkKey(%q) err=%v, want ErrNotAChunkKey", bad, err)
		}
	}
}

func TestPut_ConcurrentRace_OnlyOneActualWrite(t *testing.T) {
	c := newCAS(t)
	body := bytes.Repeat([]byte("x"), 1024)

	const N = 32
	var freshWrites atomic.Int32
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			info, err := c.PutChunk(context.Background(), body)
			if err != nil {
				t.Errorf("put: %v", err)
				return
			}
			if !info.Deduped {
				freshWrites.Add(1)
			}
		}()
	}
	wg.Wait()

	// At most one fresh write should occur (the in-memory seen cache may
	// also short-circuit subsequent calls, but the storage IfNotExists
	// guarantees correctness regardless of cache state).
	if got := freshWrites.Load(); got != 1 {
		t.Errorf("expected exactly 1 fresh write across %d concurrent puts; got %d", N, got)
	}
}

func TestGetChunkBytes_DetectsCorruption(t *testing.T) {
	// Build a CAS, put a chunk, then corrupt the on-disk bytes via the
	// underlying StoragePlugin and confirm GetChunkBytes reports the
	// checksum mismatch instead of silently returning bad data.
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	c := repo.NewCAS(sp)

	body := []byte("we will corrupt this on disk")
	info, err := c.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}

	// Overwrite with a different plaintext wrapped in a valid envelope.
	// This is more realistic than randomly-bytewise-corrupted on-disk
	// content (which the envelope-decode would catch earlier as
	// ErrCorruptEnvelope) — it's "the storage backend served a chunk
	// that decodes fine but doesn't match its content-address."
	corrupted := []byte("totally different bytes, same key")
	corruptedEnvelope := compression.WriteEnvelope(compression.AlgoNone, compression.EncryptionFields{}, corrupted)
	_, err = sp.Put(context.Background(), repo.ChunkKey(info.Hash),
		bytes.NewReader(corruptedEnvelope), storage.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.GetChunkBytes(context.Background(), info.Hash); err == nil {
		t.Fatal("GetChunkBytes should detect corruption")
	} else if !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Errorf("expected ErrChecksumMismatch; got %v", err)
	}
}

// customCodec is a Compressor that uses an algorithm ID outside the
// shipped values. It exists to prove the read path works for codecs
// the operator brings themselves — i.e. that NewCAS auto-registers
// the writer's algorithm on the read side without the caller having
// to pre-populate the registry.
type customCodec struct{}

func (customCodec) Name() string                       { return "custom" }
func (customCodec) Algorithm() compression.AlgorithmID { return compression.AlgorithmID(0xCC) }
func (customCodec) Compress(b []byte) ([]byte, compression.AlgorithmID, error) {
	// Reverse the bytes — silly but distinct from the input so a
	// missing decompressor would surface as a SHA-256 mismatch
	// rather than a coincidentally-correct round-trip.
	out := make([]byte, len(b))
	for i := range b {
		out[i] = b[len(b)-1-i]
	}
	return out, compression.AlgorithmID(0xCC), nil
}
func (customCodec) Decompress(b []byte) ([]byte, error) {
	out := make([]byte, len(b))
	for i := range b {
		out[i] = b[len(b)-1-i]
	}
	return out, nil
}

func TestNewCAS_AutoRegistersWritersAlgorithm(t *testing.T) {
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	c := repo.NewCAS(sp, repo.WithCompressor(customCodec{}))
	body := []byte("the quick brown fox jumps over the lazy dog")
	info, err := c.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatalf("PutChunk: %v", err)
	}
	got, err := c.GetChunkBytes(context.Background(), info.Hash)
	if err != nil {
		t.Fatalf("GetChunkBytes (auto-register failure?): %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("round-trip differs: got %q, want %q", got, body)
	}
}

func TestNewCAS_NilPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil StoragePlugin")
		}
	}()
	repo.NewCAS(nil)
}
