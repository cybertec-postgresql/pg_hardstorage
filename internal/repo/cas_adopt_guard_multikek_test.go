package repo_test

// cas_adopt_guard_multikek_test.go — the adopt guard in the
// configuration it exists for: a repository holding chunks under more
// than one KEK.
//
// The guard probes ONCE per CAS, on the reasoning that readability is
// "a property of (repo, DEK), not of any single chunk". That holds for
// a repository whose chunks all came from one KEK. It does not hold
// for a mixed one — and a mixed repository is exactly what the guard
// defends, since per-tenant KEKs sharing a repo is a supported,
// compliance-load-bearing setup.
//
// In a mixed repo the answer depends on WHICH chunk is adopted. A
// backup that adopted one of its OWN chunks first resolved the guard
// OK, and every later adoption of another tenant's chunk went
// unchecked: the manifest committed references to chunks the backup
// cannot decrypt, exit 0, failure only at restore. The same defect the
// guard was written to prevent, reached by adopting in a different
// order.
//
// The commit-time gate does not cover it either — runner's
// verifyAdoptedChunks Stats each adopted hash for EXISTENCE (the
// dedup-vs-GC race), and existence says nothing about custody.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// seedSharedDEKs writes one shared-DEK marker per KEKRef, which is what
// makes a repository multi-KEK on disk: sharedkey mints exactly one
// such object per ref. These tests drive the CAS directly rather than
// the resolver, so the layout is created here.
func seedSharedDEKs(t *testing.T, sp storage.StoragePlugin, refs ...string) {
	t.Helper()
	for _, ref := range refs {
		if _, err := sp.Put(context.Background(), "keys/shared-dek/"+ref+".json",
			strings.NewReader(`{}`), storage.PutOptions{IfNotExists: true}); err != nil {
			t.Fatalf("seed shared-dek %s: %v", ref, err)
		}
	}
}

// The regression: own chunk first, foreign chunk second.
func TestCAS_MultiKEK_ForeignAdoptionAfterOwnIsRefused(t *testing.T) {
	ctx := context.Background()
	sp := adoptTestStore(t)
	seedSharedDEKs(t, sp, "tenant-a", "tenant-b")

	mine := bytes.Repeat([]byte("tenant-B-owned-chunk"), 512)
	theirs := bytes.Repeat([]byte("tenant-A-owned-chunk"), 512)

	casA := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xA1)))
	if _, err := casA.PutChunk(ctx, theirs); err != nil {
		t.Fatalf("tenant A PutChunk: %v", err)
	}
	setupB := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xB2)))
	if _, err := setupB.PutChunk(ctx, mine); err != nil {
		t.Fatalf("tenant B seed PutChunk: %v", err)
	}

	// A fresh backup under DEK-B adopts its OWN chunk first — which
	// used to resolve the guard OK for everything that followed.
	casB := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xB2)))
	if _, err := casB.PutChunk(ctx, mine); err != nil {
		t.Fatalf("adopting its own chunk must still succeed: %v", err)
	}
	_, err := casB.PutChunk(ctx, theirs)
	if err == nil {
		t.Fatal("adopted a chunk encrypted under a DIFFERENT DEK because an earlier " +
			"readable adoption had already resolved the guard. The manifest would " +
			"reference chunks this backup cannot decrypt: exit 0, failure at restore")
	}
	for _, want := range []string{"does not decrypt with this backup's data key", "kms rotate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q — an operator needs cause and remedy:\n%s", want, err)
		}
	}
}

// A mixed repository must not become unusable: adopting one's OWN
// chunks is the common case and has to keep working, or every
// incremental backup in a multi-tenant repo breaks.
func TestCAS_MultiKEK_OwnChunksStillDedup(t *testing.T) {
	ctx := context.Background()
	sp := adoptTestStore(t)
	seedSharedDEKs(t, sp, "tenant-a", "tenant-b")

	casA := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xA1)))
	if _, err := casA.PutChunk(ctx, bytes.Repeat([]byte("A-only"), 512)); err != nil {
		t.Fatalf("tenant A PutChunk: %v", err)
	}

	enc := func() []byte { return bytes.Repeat([]byte("B-owned"), 512) }
	setupB := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xB2)))
	if _, err := setupB.PutChunk(ctx, enc()); err != nil {
		t.Fatalf("tenant B seed: %v", err)
	}
	casB := repo.NewCAS(sp, repo.WithEncryptor(adoptEncryptor(t, 0xB2)))
	for i := 0; i < 3; i++ {
		if _, err := casB.PutChunk(ctx, enc()); err != nil {
			t.Fatalf("repeat adoption %d of its own chunk failed: %v", i, err)
		}
	}
}

// The cost property, stated as a contract rather than left implicit:
// a single-KEK repository still probes exactly once, so the fix buys
// correctness in mixed repos without charging the common case.
func TestCAS_SingleKEK_StillProbesOnce(t *testing.T) {
	ctx := context.Background()
	backing := adoptTestStore(t)
	seedSharedDEKs(t, backing, "only-tenant")
	counter := &probeStore{StoragePlugin: backing}

	writer := repo.NewCAS(backing, repo.WithEncryptor(adoptEncryptor(t, 0xD4)))
	var bodies [][]byte
	for i := 0; i < 5; i++ {
		b := bytes.Repeat([]byte{byte('a' + i)}, 4096)
		bodies = append(bodies, b)
		if _, err := writer.PutChunk(ctx, b); err != nil {
			t.Fatalf("seed PutChunk: %v", err)
		}
	}
	reader := repo.NewCAS(counter, repo.WithEncryptor(adoptEncryptor(t, 0xD4)))
	for _, b := range bodies {
		if _, err := reader.PutChunk(ctx, b); err != nil {
			t.Fatalf("adopting PutChunk: %v", err)
		}
	}
	if got := counter.Gets(); got != 1 {
		t.Errorf("single-KEK repo issued %d probe Get(s) over 5 adoptions, want 1 — "+
			"the multi-KEK check must not cost the common case", got)
	}
}
