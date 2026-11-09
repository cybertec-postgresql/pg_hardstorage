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
	if len(seen) != n {
		t.Fatalf("done reported %d files, want %d", len(seen), n)
	}
	for p, c := range seen {
		if c != 1 {
			t.Fatalf("%s reported done %d times", p, c)
		}
	}
	// Peak concurrency, not wall time: a timing bound fails on a loaded
	// host even when files overlap.
	if p := sp.peak.Load(); p < materializeConcurrency/2 {
		t.Fatalf("peak concurrent chunk fetches = %d; files are not overlapping", p)
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

// Issue #97: within one file the chunks were read one after the other,
// so a restore whose remaining work is a few large relation segments
// ran at one storage round trip at a time. The chunks of a file are now
// read ahead, and still land in the file in list order.
func TestMaterializeFile_ReadsAheadWithinOneFile(t *testing.T) {
	fsp := &fs.Plugin{}
	if err := fsp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsp.Close() })
	sp := &slowGetSP{StoragePlugin: fsp, delay: 10 * time.Millisecond}
	w := repo.NewCAS(sp)
	f := backup.FileEntry{Path: "base/1/16384", Mode: 0o600}
	var want []byte
	for i := 0; i < 120; i++ {
		body := []byte(fmt.Sprintf("segment chunk %03d|%s", i, strings.Repeat("y", i%7)))
		info, err := w.PutChunk(context.Background(), body)
		if err != nil {
			t.Fatal(err)
		}
		f.Chunks = append(f.Chunks, backup.ChunkRef{Hash: info.Hash, Offset: int64(len(want)), Len: info.Size})
		want = append(want, body...)
	}
	f.Size = int64(len(want))

	cas := repo.NewCAS(sp, repo.WithFetchConcurrency(8))
	dest := t.TempDir()
	n, k, err := materializeFile(context.Background(), cas, dest, &f, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, f.Path))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) || n != f.Size || k != len(f.Chunks) {
		t.Fatalf("restored file differs from the chunk list in order (bytes=%d chunks=%d)", n, k)
	}
	if p := sp.peak.Load(); p < 4 || p > 8 {
		t.Fatalf("peak concurrent chunk reads within one file = %d, want 4..8", p)
	}
}

// --fetch-concurrency bounds the reads of ALL files being written, not
// each file's.
func TestMaterializeAll_FetchBudgetCoversAllFiles(t *testing.T) {
	cas, sp, m, bodies := parallelFixture(t, 64)
	cas.SetFetchConcurrency(3)
	dest := t.TempDir()
	if err := materializeAll(context.Background(), cas, m, false,
		func(*backup.FileEntry) (string, bool, error) { return dest, false, nil },
		func(*backup.FileEntry, int64, int) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for p, body := range bodies {
		if got, err := os.ReadFile(filepath.Join(dest, p)); err != nil || string(got) != string(body) {
			t.Fatalf("%s: err=%v", p, err)
		}
	}
	if p := sp.peak.Load(); p > 3 {
		t.Fatalf("peak concurrent chunk reads = %d with a budget of 3", p)
	}
}

// A chunk that cannot be read fails the file at that chunk: bytes
// written and chunks fetched stop there, as they did serially.
func TestMaterializeFile_FailsAtTheMissingChunk(t *testing.T) {
	cas, _, m, _ := parallelFixture(t, 12)
	f := backup.FileEntry{Path: "base/1/1", Mode: 0o600}
	var off int64
	for i := range m.Files {
		c := m.Files[i].Chunks[0]
		c.Offset = off
		off += c.Len
		f.Chunks = append(f.Chunks, c)
	}
	f.Size = off
	f.Chunks[5].Hash = repo.Hash{0xde, 0xad}
	_, k, err := materializeFile(context.Background(), cas, t.TempDir(), &f, false)
	if err == nil || !errors.Is(err, storage.ErrNotFound) || k != 5 {
		t.Fatalf("err=%v chunks=%d; want a not-found error after 5 chunks", err, k)
	}
}
