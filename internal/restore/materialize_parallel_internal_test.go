package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// slowGetSP delays every Get and records the peak number running at
// once: a stand-in for chunk fetches from a remote store.
type slowGetSP struct {
	storage.StoragePlugin
	delay        time.Duration
	active, peak atomic.Int32
}

func (s *slowGetSP) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	n := s.active.Add(1)
	defer s.active.Add(-1)
	for {
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(s.delay)
	return s.StoragePlugin.Get(ctx, key)
}

func parallelFixture(t *testing.T, n int) (*repo.CAS, *slowGetSP, *backup.Manifest, map[string][]byte) {
	t.Helper()
	fsp := &fs.Plugin{}
	if err := fsp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsp.Close() })
	sp := &slowGetSP{StoragePlugin: fsp, delay: 15 * time.Millisecond}
	cas := repo.NewCAS(sp)
	m := &backup.Manifest{}
	bodies := map[string][]byte{}
	for i := 0; i < n; i++ {
		body := []byte(fmt.Sprintf("file %d %s", i, strings.Repeat("x", i)))
		path := fmt.Sprintf("base/1/%d", 30000+i)
		info, err := cas.PutChunk(context.Background(), body)
		if err != nil {
			t.Fatal(err)
		}
		m.Files = append(m.Files, backup.FileEntry{Path: path, Size: int64(len(body)), Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Len: info.Size}}})
		bodies[path] = body
	}
	return repo.NewCAS(sp), sp, m, bodies // fresh CAS: no seen-cache hits
}

// Files are written concurrently (issue #90), each reported done
// exactly once, after it is fully on disk.
func TestMaterializeAll_OverlapsFilesAndReportsEachOnce(t *testing.T) {
	const n = 64
	cas, sp, m, bodies := parallelFixture(t, n)
	dest := t.TempDir()
	var mu sync.Mutex
	seen := map[string]int{}
	start := time.Now()
	err := materializeAll(context.Background(), cas, m, false,
		func(f *backup.FileEntry) (string, bool, error) { return dest, false, nil },
		func(f *backup.FileEntry, bw int64, ck int) error {
			mu.Lock()
			defer mu.Unlock()
			seen[f.Path]++
			got, err := os.ReadFile(filepath.Join(dest, f.Path))
			if err != nil || string(got) != string(bodies[f.Path]) || bw != int64(len(got)) || ck != 1 {
				return fmt.Errorf("%s reported done before it was fully written (err=%v bw=%d)", f.Path, err, bw)
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if len(seen) != n {
		t.Fatalf("done reported %d files, want %d", len(seen), n)
	}
	for p, c := range seen {
		if c != 1 {
			t.Fatalf("%s reported done %d times", p, c)
		}
	}
	if p := sp.peak.Load(); p < materializeConcurrency/2 {
		t.Fatalf("peak concurrent chunk fetches = %d; files are not overlapping", p)
	}
	if serial := time.Duration(n) * sp.delay; elapsed > serial/3 {
		t.Errorf("%d files took %v; serial would be %v", n, elapsed, serial)
	}
}

// The first failure is returned and stops the walk; files not yet
// started are never written, and nothing is reported done twice.
func TestMaterializeAll_FirstErrorStops(t *testing.T) {
	const n = 200
	cas, _, m, _ := parallelFixture(t, n)
	m.Files[20].Chunks[0].Len++ // length mismatch → materializeFile fails
	dest := t.TempDir()
	var done atomic.Int32
	err := materializeAll(context.Background(), cas, m, true,
		func(f *backup.FileEntry) (string, bool, error) { return dest, false, nil },
		func(*backup.FileEntry, int64, int) error { done.Add(1); return nil })
	if err == nil || !strings.Contains(err.Error(), m.Files[20].Path) {
		t.Fatalf("err = %v; want one naming %s", err, m.Files[20].Path)
	}
	if d := int(done.Load()); d >= n {
		t.Fatalf("all %d files reported done despite the failure", d)
	}
	if _, serr := os.Stat(filepath.Join(dest, m.Files[n-1].Path)); serr == nil {
		t.Errorf("the last file was written after the walk should have stopped")
	}
}

// plan's error and skip are honoured.
func TestMaterializeAll_PlanSkipAndError(t *testing.T) {
	cas, _, m, _ := parallelFixture(t, 30)
	dest := t.TempDir()
	var done atomic.Int32
	err := materializeAll(context.Background(), cas, m, false,
		func(f *backup.FileEntry) (string, bool, error) { return dest, strings.HasSuffix(f.Path, "0"), nil },
		func(*backup.FileEntry, int64, int) error { done.Add(1); return nil })
	if err != nil || done.Load() != 27 {
		t.Fatalf("skip: err=%v done=%d, want nil/27", err, done.Load())
	}
	boom := errors.New("boom")
	err = materializeAll(context.Background(), cas, m, false,
		func(f *backup.FileEntry) (string, bool, error) {
			if f.Path == m.Files[10].Path {
				return "", false, boom
			}
			return dest, false, nil
		},
		func(*backup.FileEntry, int64, int) error { return nil })
	if !errors.Is(err, boom) {
		t.Fatalf("plan error: got %v", err)
	}
}
