package repo_test

// PutChunk's fast path returns Deduped straight out of the `seen`
// cache, with no adoptability check:
//
//	if _, ok := c.seen.Load(hash); ok {
//	    c.dedupInMem.Add(1)
//	    info.Deduped = true
//	    return info, nil
//	}
//
// That is correct only while every entry in `seen` means "present AND
// readable by THIS CAS's DEK". Every writer honours that: the hint path
// and the ErrAlreadyExists path both call ensureAdoptable first, a
// successful Put is our own bytes, and GetChunkBytes only caches after
// a verified read.
//
// HasChunk did not. It marked a chunk seen on a bare Stat, so a caller
// that asked "is this chunk here?" before writing it would prime the
// cache with a chunk written under someone else's DEK — and the next
// PutChunk would dedup against it, committing a manifest referencing
// chunks it cannot decrypt. The backup exits 0 and fails at restore,
// which is exactly the defect cas_adopt_guard_test.go exists to
// prevent, reachable by a different door.
//
// Nothing called HasChunk, so the door was shut by accident rather than
// by design. This test holds it shut.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func TestCAS_HasChunkDoesNotPrimeDedupPastTheAdoptGuard(t *testing.T) {
	ctx := context.Background()
	sp := adoptTestStore(t)
	body := bytes.Repeat([]byte("has-chunk-then-put"), 512)

	// A chunk written under one DEK.
	first := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xA1)))
	if _, err := first.PutChunk(ctx, body); err != nil {
		t.Fatalf("first PutChunk: %v", err)
	}

	// A second backup under a DIFFERENT DEK asks whether the chunk is
	// present — the natural thing for a pre-pass or a hint builder to
	// do — and only then writes it.
	second := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xB2)))
	present, err := second.HasChunk(ctx, repo.Hash(sha256Of(body)))
	if err != nil {
		t.Fatalf("HasChunk: %v", err)
	}
	if !present {
		t.Fatal("HasChunk did not find a chunk that is in the store")
	}

	_, err = second.PutChunk(ctx, body)
	if err == nil {
		t.Fatal("PutChunk deduped against a chunk encrypted under a different DEK because " +
			"HasChunk had primed the seen cache — the fast path skips ensureAdoptable, so " +
			"the manifest commits references to chunks it cannot decrypt and the backup " +
			"fails only at restore")
	}
	if !strings.Contains(err.Error(), "does not decrypt with this backup's data key") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

// The guard must not cost ordinary dedup: same DEK, HasChunk then
// PutChunk still deduplicates.
func TestCAS_HasChunkThenPutStillDedupsUnderOneDEK(t *testing.T) {
	ctx := context.Background()
	sp := adoptTestStore(t)
	body := bytes.Repeat([]byte("same-dek-has-then-put"), 512)

	first := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xA1)))
	if _, err := first.PutChunk(ctx, body); err != nil {
		t.Fatal(err)
	}

	second := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xA1)))
	if _, err := second.HasChunk(ctx, repo.Hash(sha256Of(body))); err != nil {
		t.Fatal(err)
	}
	info, err := second.PutChunk(ctx, body)
	if err != nil {
		t.Fatalf("PutChunk under the same DEK must succeed: %v", err)
	}
	if !info.Deduped {
		t.Error("a chunk already in the repo under the SAME DEK was re-uploaded — every " +
			"incremental backup depends on adopting its predecessor's chunks")
	}
}

func sha256Of(b []byte) [32]byte { return sha256.Sum256(b) }
