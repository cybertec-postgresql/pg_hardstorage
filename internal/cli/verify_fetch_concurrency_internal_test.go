package cli

import (
	"context"
	"io"
	"net/url"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// roundTrips delays every Get and Stat and records the peak overlap:
// an object store at a few milliseconds per request.
type roundTrips struct {
	storage.StoragePlugin
	active, peak atomic.Int32
}

func (r *roundTrips) enter() func() {
	n := r.active.Add(1)
	for {
		p := r.peak.Load()
		if n <= p || r.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond)
	return func() { r.active.Add(-1) }
}

func (r *roundTrips) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	defer r.enter()()
	return r.StoragePlugin.Get(ctx, key)
}

func (r *roundTrips) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	defer r.enter()()
	return r.StoragePlugin.Stat(ctx, key)
}

// Issue #97: verify read one chunk at a time (~24 chunks/s from an
// object store). It now reads several at once, and its findings are
// what the serial walk reported: exact counts, mismatches in ascending
// hash order.
func TestVerify_ReadsChunksConcurrently_SameFindings(t *testing.T) {
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	w := casdefault.New(sp)
	m := &backup.Manifest{Files: []backup.FileEntry{{}}}
	var lost []repo.Hash
	for i := 0; i < 150; i++ {
		body := []byte{byte(i), byte(i >> 8), 'v', 'e', 'r', 'i', 'f', 'y'}
		info, err := w.PutChunk(context.Background(), body)
		if err != nil {
			t.Fatal(err)
		}
		m.Files[0].Chunks = append(m.Files[0].Chunks, backup.ChunkRef{Hash: info.Hash, Len: int64(len(body))})
		if i%10 == 3 {
			if err := sp.Delete(context.Background(), repo.ChunkKey(info.Hash)); err != nil {
				t.Fatal(err)
			}
			lost = append(lost, info.Hash)
		}
	}
	slices.SortFunc(lost, func(a, b repo.Hash) int { return slices.Compare(a[:], b[:]) })

	for _, existenceOnly := range []bool{false, true} {
		slow := &roundTrips{StoragePlugin: sp}
		var stats verifyStats
		var err error
		if existenceOnly {
			stats, err = verifyChunkExistence(context.Background(), slow, m, 0, 8)
		} else {
			cas := casdefault.New(slow)
			cas.SetFetchConcurrency(8)
			stats, err = verifyChunks(context.Background(), cas, m, 0)
		}
		if err != nil {
			t.Fatalf("existenceOnly=%v: %v", existenceOnly, err)
		}
		if stats.ChunksSampled != 150 || stats.ChunksVerified != 135 || stats.MismatchCount != 15 {
			t.Errorf("existenceOnly=%v: sampled=%d verified=%d mismatched=%d, want 150/135/15",
				existenceOnly, stats.ChunksSampled, stats.ChunksVerified, stats.MismatchCount)
		}
		if !slices.Equal(stats.Mismatches, lost) {
			t.Errorf("existenceOnly=%v: mismatches are not the missing chunks in ascending order", existenceOnly)
		}
		if p := slow.peak.Load(); p < 4 || p > 8 {
			t.Errorf("existenceOnly=%v: peak concurrent requests = %d, want 4..8", existenceOnly, p)
		}
	}
}
