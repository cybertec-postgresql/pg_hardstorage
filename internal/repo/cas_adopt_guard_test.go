package repo_test

// cas_adopt_guard_test.go — dedup must not adopt a chunk this backup's
// DEK cannot read.
//
// Chunk keys are global to a repository (chunks/sha256/<hash>.chk) but
// the shared DEK is per-KEKRef. A backup under one KEKRef that dedups
// against chunks written under another therefore commits a manifest
// referencing chunks it cannot decrypt: the backup exits 0 and fails
// only at restore.
//
// These run against fs:// so the invariant is checked in
// milliseconds, deterministically, with no container — the end-to-end
// proof lives in internal/cli's KEKRef lifecycle suite, but this is
// what pins the CAS behaviour itself.

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func adoptTestStore(t *testing.T) storage.StoragePlugin {
	t.Helper()
	p := &fs.Plugin{}
	u, err := url.Parse("file://" + t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatalf("open fs store: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func adoptEncryptor(t *testing.T, fill byte) encryption.Encryptor {
	t.Helper()
	key := make([]byte, encryption.KeyLen)
	for i := range key {
		key[i] = fill
	}
	e, err := aesgcm.New(key)
	if err != nil {
		t.Fatalf("aesgcm.New: %v", err)
	}
	return e
}

// TestCAS_DedupAcrossDEKsIsRefused is the regression test for the
// defect. Two CASes over ONE store with DIFFERENT DEKs: the second
// must refuse to adopt the first's chunk rather than silently
// deduplicating against bytes it cannot decrypt.
func TestCAS_DedupAcrossDEKsIsRefused(t *testing.T) {
	ctx := context.Background()
	sp := adoptTestStore(t)
	body := bytes.Repeat([]byte("cross-kek-dedup"), 512)

	first := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xA1)))
	if _, err := first.PutChunk(ctx, body); err != nil {
		t.Fatalf("first PutChunk: %v", err)
	}

	// A second backup under a different KEKRef resolves a different
	// shared DEK. Its chunker hashes the same plaintext and finds the
	// key already present.
	second := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xB2)))
	_, err := second.PutChunk(ctx, body)
	if err == nil {
		t.Fatal("PutChunk adopted a chunk encrypted under a different DEK — the " +
			"manifest would reference chunks it cannot decrypt, and the backup would " +
			"fail only at restore")
	}
	msg := err.Error()
	for _, want := range []string{
		"does not decrypt with this backup's data key",
		"kms rotate",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message lacks %q — an operator needs the cause and a "+
				"remedy, not just a failure:\n%s", want, msg)
		}
	}
}

// TestCAS_DedupWithinOneDEKStillWorks is the other half, and the one
// that matters for cost: the guard must not disturb ordinary dedup.
// Every incremental backup depends on adopting its predecessor's
// chunks, so a guard that refused those would be far worse than the
// bug it fixes.
func TestCAS_DedupWithinOneDEKStillWorks(t *testing.T) {
	ctx := context.Background()
	sp := adoptTestStore(t)
	body := bytes.Repeat([]byte("same-dek-dedup"), 512)

	enc := adoptEncryptor(t, 0xC3)
	first := repo.NewCAS(sp, repo.WithEncryptor(enc))
	if _, err := first.PutChunk(ctx, body); err != nil {
		t.Fatalf("first PutChunk: %v", err)
	}

	// A separate CAS instance with the SAME key — a later backup in the
	// same repo under the same KEKRef.
	second := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xC3)))
	info, err := second.PutChunk(ctx, body)
	if err != nil {
		t.Fatalf("dedup within one DEK was refused: %v", err)
	}
	if !info.Deduped {
		t.Error("chunk was re-written rather than deduplicated; the guard has " +
			"defeated dedup, which is the whole point of content addressing")
	}
}

// TestCAS_UnencryptedRepoUnaffected pins that the guard costs nothing
// where it cannot apply: with no DEK in play there is no cross-DEK
// question, and the probe must not run at all.
func TestCAS_UnencryptedRepoUnaffected(t *testing.T) {
	ctx := context.Background()
	sp := adoptTestStore(t)
	body := bytes.Repeat([]byte("plaintext"), 512)

	first := repo.NewCAS(sp)
	if _, err := first.PutChunk(ctx, body); err != nil {
		t.Fatalf("first PutChunk: %v", err)
	}
	second := repo.NewCAS(sp)
	info, err := second.PutChunk(ctx, body)
	if err != nil {
		t.Fatalf("unencrypted dedup refused: %v", err)
	}
	if !info.Deduped {
		t.Error("unencrypted chunk was re-written rather than deduplicated")
	}
}

// TestCAS_AdoptProbeRunsOncePerCAS pins the cost model. The check is a
// property of (repo, DEK), not of any single chunk, so it must resolve
// once — verifying every dedup hit would read and decrypt each
// deduplicated chunk, which is exactly the work dedup exists to avoid.
func TestCAS_AdoptProbeRunsOncePerCAS(t *testing.T) {
	ctx := context.Background()
	backing := adoptTestStore(t)
	counter := &countingGetStore{StoragePlugin: backing}

	enc := adoptEncryptor(t, 0xD4)
	writer := repo.NewCAS(backing, repo.WithEncryptor(enc))
	var bodies [][]byte
	for i := 0; i < 5; i++ {
		b := bytes.Repeat([]byte{byte('a' + i)}, 4096)
		bodies = append(bodies, b)
		if _, err := writer.PutChunk(ctx, b); err != nil {
			t.Fatalf("seed PutChunk: %v", err)
		}
	}

	// A fresh CAS over the counting store adopts all five.
	reader := repo.NewCAS(counter, repo.WithEncryptor(adoptEncryptor(t, 0xD4)))
	for _, b := range bodies {
		if _, err := reader.PutChunk(ctx, b); err != nil {
			t.Fatalf("adopting PutChunk: %v", err)
		}
	}
	if got := counter.gets; got != 1 {
		t.Errorf("the adopt probe issued %d Get(s) across 5 adoptions, want exactly 1 — "+
			"a per-chunk probe would read and decrypt every deduplicated chunk", got)
	}
}

// countingGetStore counts Get calls so the probe's cost is observable.
type countingGetStore struct {
	storage.StoragePlugin
	gets int
}

func (c *countingGetStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	c.gets++
	return c.StoragePlugin.Get(ctx, key)
}
