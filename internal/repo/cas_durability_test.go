package repo_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// newDeferredCAS builds a CAS over a fresh fs repo with chunk writes
// in DurabilityDeferred mode.
func newDeferredCAS(t *testing.T) *repo.CAS {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: t.TempDir()},
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return repo.NewCAS(sp, repo.WithChunkDurability(storage.DurabilityDeferred))
}

// A CAS in deferred mode round-trips chunks: written deferred,
// readable before the Barrier (page cache), and correct after it.
func TestCASDurability_DeferredRoundTrip(t *testing.T) {
	c := newDeferredCAS(t)
	ctx := context.Background()

	bodies := [][]byte{
		[]byte("chunk-one"),
		[]byte("chunk-two-is-longer"),
		[]byte("chunk-three"),
	}
	var infos []repo.ChunkInfo
	for _, b := range bodies {
		info, err := c.PutChunk(ctx, b)
		if err != nil {
			t.Fatalf("put deferred chunk: %v", err)
		}
		infos = append(infos, info)
	}

	// A deferred chunk is staged, NOT yet published at its key — it
	// is not readable until the Barrier (the crash-safety contract).
	if _, err := c.GetChunkBytes(ctx, infos[0].Hash); err == nil {
		t.Error("deferred chunk readable before Barrier — staging contract violated")
	}

	// Barrier publishes + durably commits the deferred writes.
	if err := c.Barrier(ctx); err != nil {
		t.Fatalf("barrier: %v", err)
	}
	for i, info := range infos {
		got, err := c.GetChunkBytes(ctx, info.Hash)
		if err != nil {
			t.Fatalf("get chunk %d after barrier: %v", i, err)
		}
		if string(got) != string(bodies[i]) {
			t.Errorf("chunk %d after barrier: got %q, want %q", i, got, bodies[i])
		}
	}
}

// Barrier on the default (inline) CAS is a safe no-op — every
// PutChunk was already durable.
func TestCASDurability_InlineBarrierIsNoop(t *testing.T) {
	c := newCAS(t)
	ctx := context.Background()
	if _, err := c.PutChunk(ctx, []byte("inline-chunk")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := c.Barrier(ctx); err != nil {
		t.Errorf("inline barrier: %v", err)
	}
}
