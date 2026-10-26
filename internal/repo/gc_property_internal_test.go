package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	fsplug "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
)

// propWorld is a randomly generated repository plus the model of what a
// correct gc must keep and delete.
type propWorld struct {
	root   string
	sp     storage.StoragePlugin
	keep   map[Hash]string // chunks that must survive, with why
	reap   map[Hash]string // chunks gc --apply must delete
	staleK map[string]bool // stale temps that must be reaped
	keepK  map[string]bool // temps / other objects that must survive
}

func propOpen(t testing.TB, root string) storage.StoragePlugin {
	t.Helper()
	sp := &fsplug.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

func propPut(t testing.TB, sp storage.StoragePlugin, root, key, body string, age time.Duration) {
	t.Helper()
	if err := putJSONRaw(sp, key, body); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(key)), old, old); err != nil {
		t.Fatal(err)
	}
}

func chunkJSON(hs []Hash) string {
	var p []string
	for _, h := range hs {
		p = append(p, `{"hash":"`+h.String()+`"}`)
	}
	return strings.Join(p, ",")
}

func genWorld(t testing.TB, seed int64) *propWorld {
	rng := rand.New(rand.NewSource(seed))
	root := t.TempDir()
	sp := propOpen(t, root)
	w := &propWorld{root: root, sp: sp, keep: map[Hash]string{}, reap: map[Hash]string{},
		staleK: map[string]bool{}, keepK: map[string]bool{}}
	ctx := context.Background()
	n := 0
	mk := func(age time.Duration) Hash {
		n++
		ci, err := NewCAS(sp).PutChunk(ctx, []byte(fmt.Sprintf("seed%d-chunk%d-%d", seed, n, rng.Int())))
		if err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(ChunkKey(ci.Hash))), old, old); err != nil {
			t.Fatal(err)
		}
		return ci.Hash
	}
	const old = 72 * time.Hour
	pick := func(k int) []Hash {
		var out []Hash
		for i := 0; i < k; i++ {
			out = append(out, mk(old))
		}
		return out
	}
	deps := []string{"db1", "db2", "db.json.tmp.x"}
	// live backups
	var shared []Hash
	for i, nb := 0, 1+rng.Intn(4); i < nb; i++ {
		dep := deps[rng.Intn(len(deps))]
		hs := pick(1 + rng.Intn(6))
		if len(shared) > 0 && rng.Intn(2) == 0 {
			hs = append(hs, shared[rng.Intn(len(shared))])
		}
		shared = append(shared, hs...)
		for _, h := range hs {
			w.keep[h] = "live-backup"
		}
		half := len(hs) / 2
		body := fmt.Sprintf(`{"files":[{"chunks":[%s]},{"chunks":[%s]}]}`, chunkJSON(hs[:half]), chunkJSON(hs[half:]))
		propPut(t, sp, root, fmt.Sprintf("manifests/%s/backups/live%d/manifest.json", dep, i), body, old)
	}
	// wal + logical
	for i, nw := 0, rng.Intn(4); i < nw; i++ {
		hs := pick(1 + rng.Intn(3))
		for _, h := range hs {
			w.keep[h] = "wal"
		}
		propPut(t, sp, root, fmt.Sprintf("wal/db1/1/%024d.json", i), `{"chunks":[`+chunkJSON(hs)+`]}`, old)
	}
	for i, nw := 0, rng.Intn(3); i < nw; i++ {
		hs := pick(1 + rng.Intn(3))
		for _, h := range hs {
			w.keep[h] = "logical"
		}
		propPut(t, sp, root, fmt.Sprintf("logical/db1/s1/%016x.json", i), `{"chunks":[`+chunkJSON(hs)+`]}`, old)
	}
	// tombstoned backups: old tombstone => reap, young => keep. Same id in
	// another deployment stays live.
	for i, nt := 0, rng.Intn(4); i < nt; i++ {
		id := fmt.Sprintf("tomb%d", i)
		hs := pick(1 + rng.Intn(4))
		propPut(t, sp, root, "manifests/db1/backups/"+id+"/manifest.json", `{"files":[{"chunks":[`+chunkJSON(hs)+`]}]}`, old)
		young := rng.Intn(2) == 0
		age := old
		if young {
			age = time.Hour
		}
		propPut(t, sp, root, "manifests/db1/backups/"+id+"/manifest.json.tombstone", `{}`, age)
		why := "tombstoned-old"
		dst := w.reap
		if young {
			why, dst = "tombstoned-young", w.keep
		}
		for _, h := range hs {
			if _, k := w.keep[h]; k {
				continue
			}
			dst[h] = why
		}
		if rng.Intn(3) == 0 { // same id, other deployment, live
			h2 := pick(1)
			w.keep[h2[0]] = "same-id-other-dep"
			propPut(t, sp, root, "manifests/db2/backups/"+id+"/manifest.json", `{"files":[{"chunks":[`+chunkJSON(h2)+`]}]}`, old)
		}
	}
	// A hash shared between a live and a dead backup must be kept.
	for h := range w.keep {
		delete(w.reap, h)
	}
	// orphans
	for i, no := 0, rng.Intn(30); i < no; i++ {
		if rng.Intn(4) == 0 {
			w.keep[mk(time.Minute)] = "young-orphan"
		} else {
			w.reap[mk(old)] = "orphan"
		}
	}
	// non-chunk junk under chunks/: must not be touched
	propPut(t, sp, root, "chunks/sha256/zz/notachunk", "x", old)
	w.keepK["chunks/sha256/zz/notachunk"] = true
	// stale temps
	for i, nt := 0, rng.Intn(4); i < nt; i++ {
		k := fmt.Sprintf("manifests/db1/backups/x%d/manifest.json.tmp.abc%d", i, i)
		if rng.Intn(2) == 0 {
			propPut(t, sp, root, k, "partial", old)
			w.staleK[k] = true
		} else {
			propPut(t, sp, root, k, "partial", time.Minute)
			w.keepK[k] = true
		}
	}
	return w
}

func snapTree(t testing.TB, root string) (files []string, emptyDirs []string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			ents, _ := os.ReadDir(p)
			if len(ents) == 0 && rel != "." {
				emptyDirs = append(emptyDirs, rel)
			}
			return nil
		}
		files = append(files, rel)
		return nil
	})
	sort.Strings(files)
	return
}

func propFast(t testing.TB) {
	saved := [4]time.Duration{GCFenceSettle, GCFenceWriterBudget, gcFencePoll, GCFenceMaxWait}
	savedMem := sweepOrphanMemRecords
	GCFenceSettle, GCFenceWriterBudget, gcFencePoll, GCFenceMaxWait = 0, time.Hour, 5*time.Millisecond, 10*time.Second
	t.Cleanup(func() {
		GCFenceSettle, GCFenceWriterBudget, gcFencePoll, GCFenceMaxWait = saved[0], saved[1], saved[2], saved[3]
		sweepOrphanMemRecords = savedMem
	})
}

func assertEmptyDir(t testing.TB, dir, what string) {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("%s: leftover files in %s: %v", what, dir, names)
	}
}

func TestProp_SweepModel(t *testing.T) {
	propFast(t)
	seeds := 150
	if testing.Short() {
		seeds = 20
	}
	for seed := int64(1); seed <= int64(seeds); seed++ {
		seed := seed
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed * 7919))
			w := genWorld(t, seed)
			spillDir := t.TempDir()
			sweepOrphanMemRecords = 1 + rng.Intn(8)
			opts := SweepOptions{
				RefSpill:          spill.Options{Dir: spillDir, MemEntries: 1 + rng.Intn(6)},
				BatchSize:         1 + rng.Intn(9),
				DeleteConcurrency: 1 + rng.Intn(5),
				BatchDuration:     time.Duration(rng.Intn(3)) * time.Millisecond,
			}
			ctx := context.Background()
			dry, err := Sweep(ctx, w.sp, opts)
			if err != nil {
				t.Fatal(err)
			}
			if dry.OrphanCount != len(w.reap) {
				t.Fatalf("dry orphan count %d want %d", dry.OrphanCount, len(w.reap))
			}
			if len(dry.StaleTemps) != len(w.staleK) {
				t.Fatalf("dry stale %v want %v", dry.StaleTemps, w.staleK)
			}
			before, _ := snapTree(t, w.root)
			assertEmptyDir(t, spillDir, "after dry run")
			opts.Apply = true
			res, err := Sweep(ctx, w.sp, opts)
			if err != nil {
				t.Fatal(err)
			}
			if res.OrphanCount != dry.OrphanCount || res.RefCount != dry.RefCount ||
				res.OrphanBytes != dry.OrphanBytes || fmt.Sprint(res.StaleTemps) != fmt.Sprint(dry.StaleTemps) ||
				fmt.Sprint(res.Orphans) != fmt.Sprint(dry.Orphans) {
				t.Fatalf("dry != real:\n%+v\n%+v", dry, res)
			}
			if res.Deleted != len(w.reap) || res.FailureCount != 0 || res.StaleTempDeleted != len(w.staleK) {
				t.Fatalf("apply result %+v want %d deleted", res, len(w.reap))
			}
			for h, why := range w.keep {
				if !chunkExists(t, w.sp, h) {
					t.Errorf("chunk %s (%s) was deleted", h, why)
				}
			}
			for h, why := range w.reap {
				if chunkExists(t, w.sp, h) {
					t.Errorf("chunk %s (%s) survived", h, why)
				}
			}
			for k := range w.staleK {
				if _, err := w.sp.Stat(ctx, k); !errors.Is(err, storage.ErrNotFound) {
					t.Errorf("stale temp %s survived", k)
				}
			}
			for k := range w.keepK {
				if _, err := w.sp.Stat(ctx, k); err != nil {
					t.Errorf("%s was deleted: %v", k, err)
				}
			}
			assertEmptyDir(t, spillDir, "after apply")
			// no run-heartbeat goroutine leak / state
			files, empties := snapTree(t, w.root)
			_ = before
			for _, f := range files {
				if strings.Contains(f, ".hstmp-") || strings.Contains(f, ".deferred-") || strings.Contains(f, ".excl-") {
					t.Errorf("staging leftover %s", f)
				}
			}
			for _, e := range empties {
				t.Errorf("empty dir left behind: %s", e)
			}
			// second run is a no-op
			files1, _ := snapTree(t, w.root)
			res2, err := Sweep(ctx, w.sp, opts)
			if err != nil {
				t.Fatal(err)
			}
			if res2.OrphanCount != 0 || res2.Deleted != 0 || res2.StaleTempDeleted != 0 || len(res2.StaleTemps) != 0 {
				t.Fatalf("second gc not a no-op: %+v", res2)
			}
			files2, _ := snapTree(t, w.root)
			// only the new run record may differ
			diff := symDiff(files1, files2)
			for _, d := range diff {
				if !strings.HasPrefix(d, "gc/runs/") {
					t.Errorf("second gc changed %s", d)
				}
			}
			assertEmptyDir(t, spillDir, "after second apply")
		})
	}
}

func symDiff(a, b []string) []string {
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	var out []string
	for k, v := range m {
		if v != 0 {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// faultSP injects errors into a wrapped plugin.
type faultSP struct {
	storage.StoragePlugin
	mu       sync.Mutex
	rng      *rand.Rand
	pDelete  float64
	pList    float64
	pGet     float64
	pPut     float64
	deletes  atomic.Int64
	killAt   int64 // after this many successful deletes, every op fails (crash)
	crashed  atomic.Bool
	deleted  map[string]bool
	failHere func(op, key string) error
}

var errInjected = errors.New("injected fault")

func (f *faultSP) roll(p float64) bool {
	if p <= 0 {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rng.Float64() < p
}
func (f *faultSP) dead() error {
	if f.crashed.Load() {
		return errors.New("injected crash")
	}
	return nil
}
func (f *faultSP) Delete(ctx context.Context, key string) error {
	if err := f.dead(); err != nil {
		return err
	}
	if f.roll(f.pDelete) {
		return errInjected
	}
	if f.killAt > 0 && f.deletes.Add(1) > f.killAt {
		f.crashed.Store(true)
		return errors.New("injected crash")
	}
	return f.StoragePlugin.Delete(ctx, key)
}
func (f *faultSP) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := f.dead(); err != nil {
		return nil, err
	}
	if f.roll(f.pGet) {
		return nil, errInjected
	}
	return f.StoragePlugin.Get(ctx, key)
}
func (f *faultSP) Put(ctx context.Context, key string, r io.Reader, o storage.PutOptions) (storage.PutResult, error) {
	if err := f.dead(); err != nil {
		return storage.PutResult{}, err
	}
	if f.roll(f.pPut) {
		return storage.PutResult{}, errInjected
	}
	return f.StoragePlugin.Put(ctx, key, r, o)
}
func (f *faultSP) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	return func(yield func(storage.ObjectInfo, error) bool) {
		if err := f.dead(); err != nil {
			yield(storage.ObjectInfo{}, err)
			return
		}
		if f.roll(f.pList) {
			yield(storage.ObjectInfo{}, errInjected)
			return
		}
		for i, e := range f.StoragePlugin.List(ctx, prefix) {
			if e == nil && f.roll(f.pList/50) {
				yield(storage.ObjectInfo{}, errInjected)
				return
			}
			if !yield(i, e) {
				return
			}
		}
	}
}

// TestProp_SweepFaults: random storage faults and crashes at any point
// never delete a referenced chunk; once faults stop, gc converges to the
// model state; no spill files are left behind.
func TestProp_SweepFaults(t *testing.T) {
	propFast(t)
	seeds := 120
	if testing.Short() {
		seeds = 15
	}
	for seed := int64(1); seed <= int64(seeds); seed++ {
		seed := seed
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed * 104729))
			w := genWorld(t, seed+1000)
			spillDir := t.TempDir()
			sweepOrphanMemRecords = 1 + rng.Intn(8)
			ctx := context.Background()
			for round := 0; round < 4; round++ {
				f := &faultSP{StoragePlugin: w.sp, rng: rand.New(rand.NewSource(seed*31 + int64(round)))}
				switch rng.Intn(5) {
				case 0:
					f.pDelete = 0.3
				case 1:
					f.pList = 0.1
				case 2:
					f.pGet = 0.05
				case 3:
					f.pPut = 0.3
				case 4:
					f.killAt = int64(1 + rng.Intn(10))
				}
				opts := SweepOptions{
					Apply:             true,
					RefSpill:          spill.Options{Dir: spillDir, MemEntries: 1 + rng.Intn(6)},
					BatchSize:         1 + rng.Intn(5),
					DeleteConcurrency: 1 + rng.Intn(3),
				}
				_, _ = Sweep(ctx, f, opts)
				for h, why := range w.keep {
					if !chunkExists(t, w.sp, h) {
						t.Fatalf("round %d: referenced chunk %s (%s) deleted under faults", round, h, why)
					}
				}
				assertEmptyDir(t, spillDir, fmt.Sprintf("round %d", round))
			}
			// converge
			opts := SweepOptions{Apply: true, RefSpill: spill.Options{Dir: spillDir, MemEntries: 3}}
			res, err := Sweep(ctx, w.sp, opts)
			if err != nil || res.FailureCount != 0 {
				t.Fatalf("clean run: %v %+v", err, res)
			}
			for h, why := range w.reap {
				if chunkExists(t, w.sp, h) {
					t.Errorf("orphan %s (%s) survived the clean run", h, why)
				}
			}
			for h := range w.keep {
				if !chunkExists(t, w.sp, h) {
					t.Errorf("keep %s deleted", h)
				}
			}
			res2, _ := Sweep(ctx, w.sp, opts)
			if res2.Deleted != 0 || res2.OrphanCount != 0 {
				t.Errorf("not converged: %+v", res2)
			}
			assertEmptyDir(t, spillDir, "end")
			_, empties := snapTree(t, w.root)
			for _, e := range empties {
				t.Errorf("empty dir: %s", e)
			}
		})
	}
}

// TestProp_SweepVsWriters: writers adopt (deduplicate against) orphan
// chunks and commit manifests through the commit fence while one or two
// gc --apply runs sweep. Every commit that reports success must leave all
// its chunks restorable; failures must be the typed retryable errors.
func TestProp_SweepVsWriters(t *testing.T) {
	propFast(t)
	GCFenceSettle, GCFenceWriterBudget, gcFencePoll, GCFenceMaxWait = 40*time.Millisecond, 15*time.Millisecond, 2*time.Millisecond, 20*time.Second
	seeds := 40
	if testing.Short() {
		seeds = 6
	}
	for seed := int64(1); seed <= int64(seeds); seed++ {
		seed := seed
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed * 15485863))
			w := genWorld(t, seed+5000)
			ctx := context.Background()
			var orphans []Hash
			for h := range w.reap {
				orphans = append(orphans, h)
			}
			sort.Slice(orphans, func(i, j int) bool { return orphans[i].Compare(orphans[j]) < 0 })
			type commit struct {
				key string
				hs  []Hash
			}
			var cmu sync.Mutex
			var ok []commit
			var wg sync.WaitGroup
			stop := make(chan struct{})
			nw := 1 + rng.Intn(3)
			for wi := 0; wi < nw; wi++ {
				wg.Add(1)
				go func(wi int) {
					defer wg.Done()
					r := rand.New(rand.NewSource(seed*100 + int64(wi)))
					for i := 0; ; i++ {
						select {
						case <-stop:
							return
						default:
						}
						if len(orphans) == 0 {
							return
						}
						var hs []Hash
						for k, n := 0, 1+r.Intn(4); k < n; k++ {
							hs = append(hs, orphans[r.Intn(len(orphans))])
						}
						fence, err := BeginCommitFence(ctx, w.sp, hs, FenceOptions{Owner: "w"})
						if err != nil {
							continue
						}
						miss := false
						for _, h := range hs {
							if _, err := w.sp.Stat(ctx, ChunkKey(h)); err != nil {
								miss = true
							}
						}
						if miss {
							continue
						}
						time.Sleep(time.Duration(r.Intn(3)) * time.Millisecond)
						key := fmt.Sprintf("manifests/db9/backups/w%d-%d/manifest.json", wi, i)
						if err := putJSONRaw(w.sp, key, `{"files":[{"chunks":[`+chunkJSON(hs)+`]}]}`); err != nil {
							t.Error(err)
							return
						}
						if err := fence.Confirm(ctx); err != nil {
							if !errors.Is(err, ErrAdoptedChunkSwept) {
								t.Errorf("confirm: %v", err)
							}
							// loud failure: the writer knows; remove the broken manifest
							_ = w.sp.Delete(ctx, key)
							continue
						}
						cmu.Lock()
						ok = append(ok, commit{key, hs})
						cmu.Unlock()
					}
				}(wi)
			}
			opts := SweepOptions{Apply: true, BatchSize: 1 + rng.Intn(4), DeleteConcurrency: 2,
				RefSpill: spill.Options{Dir: t.TempDir(), MemEntries: 4}}
			var sw sync.WaitGroup
			for i, n := 0, 1+rng.Intn(2); i < n; i++ {
				sw.Add(1)
				go func() {
					defer sw.Done()
					if _, err := Sweep(ctx, w.sp, opts); err != nil {
						t.Logf("sweep: %v", err)
					}
				}()
			}
			sw.Wait()
			close(stop)
			wg.Wait()
			for _, c := range ok {
				for _, h := range c.hs {
					if !chunkExists(t, w.sp, h) {
						t.Fatalf("committed manifest %s lost chunk %s", c.key, h)
					}
				}
			}
			for h, why := range w.keep {
				if !chunkExists(t, w.sp, h) {
					t.Fatalf("keep %s (%s) deleted", h, why)
				}
			}
		})
	}
}

func TestReapStaleSpill(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-10 * 24 * time.Hour)
	mkOld := func(p string, isDir bool) {
		if isDir {
			_ = os.MkdirAll(filepath.Join(dir, p), 0o755)
			_ = os.WriteFile(filepath.Join(dir, p, "run0"), []byte("x"), 0o644)
			_ = os.Chtimes(filepath.Join(dir, p, "run0"), old, old)
		} else {
			_ = os.WriteFile(filepath.Join(dir, p), []byte("x"), 0o644)
		}
		_ = os.Chtimes(filepath.Join(dir, p), old, old)
	}
	mkOld("pghs-spill-old", true)
	mkOld("pghs-orphans-old", false)
	mkOld("unrelated", false)
	_ = os.MkdirAll(filepath.Join(dir, "pghs-spill-new"), 0o755)
	reapStaleSpill(dir, time.Now(), nil)
	for _, gone := range []string{"pghs-spill-old", "pghs-orphans-old"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s survived", gone)
		}
	}
	for _, kept := range []string{"unrelated", "pghs-spill-new"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s removed", kept)
		}
	}
}
