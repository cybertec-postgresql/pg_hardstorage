package repo

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// latencySP adds a fixed round-trip latency to every per-object call,
// the shape of an object store (List pages are left free: they are few).
type latencySP struct {
	storage.StoragePlugin
	d time.Duration
}

func (l latencySP) Get(ctx context.Context, k string) (io.ReadCloser, error) {
	time.Sleep(l.d)
	return l.StoragePlugin.Get(ctx, k)
}
func (l latencySP) Stat(ctx context.Context, k string) (storage.ObjectInfo, error) {
	time.Sleep(l.d)
	return l.StoragePlugin.Stat(ctx, k)
}
func (l latencySP) Delete(ctx context.Context, k string) error {
	time.Sleep(l.d)
	return l.StoragePlugin.Delete(ctx, k)
}
func (l latencySP) Put(ctx context.Context, k string, r io.Reader, o storage.PutOptions) (storage.PutResult, error) {
	time.Sleep(l.d)
	return l.StoragePlugin.Put(ctx, k, r, o)
}

// legacyCollect is the pre-fix CollectReferences: manifests/ listed
// twice, every manifest read serially.
func legacyCollect(ctx context.Context, sp storage.StoragePlugin) (*RefSet, error) {
	refs := NewRefSet()
	for info, err := range sp.List(ctx, "manifests/") {
		if err != nil {
			return nil, err
		}
		_ = info // tombstone pass (none in the fixture)
	}
	for info, err := range sp.List(ctx, "manifests/") {
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(info.Key, "/manifest.json") {
			if err := harvestManifest(ctx, sp, info.Key, refs, harvestBackup); err != nil {
				return nil, err
			}
		}
	}
	for _, p := range []string{"wal/", "logical/"} {
		for info, err := range sp.List(ctx, p) {
			if err != nil {
				return nil, err
			}
			if strings.HasSuffix(info.Key, ".json") && !isStaleTempKey(info.Key) {
				if err := harvestManifest(ctx, sp, info.Key, refs, harvestWAL); err != nil {
					return nil, err
				}
			}
		}
	}
	return refs, nil
}

// legacyGCApply is the pre-fix `repo gc --apply` storage workload:
// collect, find orphans, find stale temps (another manifests/ + wal/
// walk), Stat every orphan serially for the size estimate, re-collect,
// then Stat+Delete each orphan 16-wide.
func legacyGCApply(ctx context.Context, sp storage.StoragePlugin) (int, error) {
	refs, err := legacyCollect(ctx, sp)
	if err != nil {
		return 0, err
	}
	orphans, err := FindOrphans(ctx, sp, refs)
	if err != nil {
		return 0, err
	}
	if _, err := FindStaleTempManifests(ctx, sp, FindOrphansOptions{MinAge: -1}); err != nil {
		return 0, err
	}
	for _, h := range orphans {
		_, _ = sp.Stat(ctx, ChunkKey(h))
	}
	refs2, err := legacyCollect(ctx, sp)
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	var mu sync.Mutex
	n := 0
	for _, h := range orphans {
		if refs2.Has(h) {
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(h Hash) {
			defer func() { <-sem; wg.Done() }()
			_, _ = sp.Stat(ctx, ChunkKey(h))
			if sp.Delete(ctx, ChunkKey(h)) == nil {
				mu.Lock()
				n++
				mu.Unlock()
			}
		}(h)
	}
	wg.Wait()
	return n, nil
}

func buildBenchRepo(tb testing.TB, deployments, backups, chunksPer, walSegs, orphans int) storage.StoragePlugin {
	tb.Helper()
	root := tb.TempDir()
	old := time.Now().Add(-72 * time.Hour)
	n := 0
	write := func(key string, body []byte) {
		p := filepath.Join(root, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			tb.Fatal(err)
		}
		_ = os.Chtimes(p, old, old)
	}
	chunk := func() Hash {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		n++
		h := Hash(sha256.Sum256(b[:]))
		write(ChunkKey(h), b[:])
		return h
	}
	refs := func(k int) string {
		var sb strings.Builder
		for c := 0; c < k; c++ {
			if c > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `{"hash":"%s"}`, chunk())
		}
		return sb.String()
	}
	for d := 0; d < deployments; d++ {
		dep := "db" + strconv.Itoa(d)
		for b := 0; b < backups; b++ {
			write(fmt.Sprintf("manifests/%s/backups/%s.full.%06d/manifest.json", dep, dep, b),
				[]byte(`{"files":[{"chunks":[`+refs(chunksPer)+`]}]}`))
		}
		for s := 0; s < walSegs; s++ {
			write(fmt.Sprintf("wal/%s/00000001/%024X.json", dep, s+1), []byte(`{"chunks":[`+refs(4)+`]}`))
		}
	}
	for o := 0; o < orphans; o++ {
		chunk()
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = sp.Close() })
	return sp
}

// TestGCSweep_LatencyBenchmark compares the pre-fix gc workload with
// Sweep on a synthetic shared repository behind a 1 ms per-object
// latency (an object store's round trip, scaled down). Opt-in:
// PG_HARDSTORAGE_GC_BENCH=1.
func TestGCSweep_LatencyBenchmark(t *testing.T) {
	if os.Getenv("PG_HARDSTORAGE_GC_BENCH") == "" {
		t.Skip("set PG_HARDSTORAGE_GC_BENCH=1")
	}
	defer func(s time.Duration) { GCFenceSettle = s }(GCFenceSettle)
	GCFenceSettle = 0
	const deployments, backups, chunksPer, walSegs, orphans = 4, 500, 40, 2500, 20000
	lat := time.Millisecond

	legacySP := latencySP{buildBenchRepo(t, deployments, backups, chunksPer, walSegs, orphans), lat}
	start := time.Now()
	n, err := legacyGCApply(context.Background(), legacySP)
	legacy := time.Since(start)
	if err != nil || n != orphans {
		t.Fatalf("legacy: deleted %d, err %v", n, err)
	}

	newSP := latencySP{buildBenchRepo(t, deployments, backups, chunksPer, walSegs, orphans), lat}
	start = time.Now()
	res, err := Sweep(context.Background(), newSP, SweepOptions{Apply: true, MinChunkAge: -1, TombstoneGrace: -1})
	sweep := time.Since(start)
	if err != nil || res.Deleted != orphans {
		t.Fatalf("sweep: deleted %d, err %v", res.Deleted, err)
	}
	t.Logf("%d manifests, %d referenced chunks, %d orphans, %v/object: legacy gc --apply %v, Sweep %v (%.1fx)",
		deployments*(backups+walSegs), deployments*(backups*chunksPer+walSegs*4), orphans, lat,
		legacy.Round(time.Millisecond), sweep.Round(time.Millisecond), float64(legacy)/float64(sweep))
}
