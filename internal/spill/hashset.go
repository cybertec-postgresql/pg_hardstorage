// Package spill holds the bounded-memory set primitives the repository
// walkers use when the number of distinct chunk hashes can exceed RAM.
//
// A 200 TB backup at the default 64 KiB average chunk size references
// about 3.3 billion distinct chunks. A Go map[[32]byte]struct{} costs
// well over 60 bytes per entry, so the GC mark set, the integrity
// check and the unique-byte counters would each need hundreds of GB.
// HashSet keeps the same Add/Has/Len contract but holds at most
// Options.MemEntries hashes on the heap; the rest live in sorted run
// files in a temp directory, merged LSM-style, each fronted by an
// in-memory Bloom filter and a sparse fence index so a negative lookup
// touches no disk and a positive one reads a single block.
package spill

import (
	"bufio"
	"bytes"
	"container/heap"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
)

// Key is a SHA-256 content hash.
type Key = [32]byte

const (
	keyLen = 32

	// DefaultMemEntries is the heap budget before a flush: 4M hashes,
	// roughly 300 MiB as a Go map.
	DefaultMemEntries = 4 << 20

	// fenceEvery is the sparse-index stride: one in-memory key per
	// fenceEvery on-disk keys, so a lookup reads one 32 KiB block.
	fenceEvery = 1024

	bloomBitsPerKey = 10
	bloomProbes     = 7
)

// Options tunes a HashSet. The zero value is usable.
type Options struct {
	// MemEntries is how many hashes stay on the heap before they are
	// flushed to a sorted run file. Zero means DefaultMemEntries.
	MemEntries int
	// Dir is where run files are created. Empty means os.TempDir().
	// The directory is only created (and only needs to be writable)
	// once the set outgrows MemEntries.
	Dir string
}

// HashSet is an exact, concurrency-safe set of hashes whose heap use is
// bounded by Options.MemEntries (plus ~1.25 bytes of Bloom filter and
// 1/1024 of a key per on-disk key). Call Close to remove spill files.
type HashSet struct {
	opts Options

	mu     sync.RWMutex
	mem    map[Key]struct{}
	runs   []*run // oldest first; levels are non-increasing (binary counter)
	dir    string // lazily created
	seq    int
	closed bool

	// retryAt is the heap size at which a flush that failed is tried
	// again. Without it every Add after a failed flush (disk full) would
	// re-sort the whole heap buffer and retry, so a full spill volume
	// would crawl instead of failing; doubling keeps the retries
	// amortised while the buffer, which cannot be shed, keeps growing.
	retryAt int

	// errMu guards err on its own: lookups run under the read lock and
	// must still be able to record a damaged run file.
	errMu sync.Mutex
	err   error // sticky spill error, surfaced by Err/Close
}

// setErr records the first spill error.
func (s *HashSet) setErr(err error) {
	s.errMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.errMu.Unlock()
}

// NewHashSet returns an empty set.
func NewHashSet(opts Options) *HashSet {
	if opts.MemEntries <= 0 {
		opts.MemEntries = DefaultMemEntries
	}
	return &HashSet{opts: opts, mem: make(map[Key]struct{})}
}

// Add inserts k. It reports whether k was absent before the call.
func (s *HashSet) Add(k Key) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.mem[k]; ok {
		return false
	}
	if s.inRunsLocked(k) {
		return false
	}
	s.mem[k] = struct{}{}
	if len(s.mem) >= s.opts.MemEntries && len(s.mem) >= s.retryAt {
		if err := s.flushLocked(); err != nil {
			s.setErr(err)
			s.retryAt = 2 * len(s.mem)
		} else {
			s.retryAt = 0
		}
	}
	return true
}

// Has reports whether k is in the set.
func (s *HashSet) Has(k Key) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.mem[k]; ok {
		return true
	}
	return s.inRunsLocked(k)
}

// Err returns the first spill I/O error. Once non-nil the set may be
// missing members, so a caller using it for a safety decision (GC)
// MUST check Err before trusting a negative Has.
func (s *HashSet) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

// Len returns the exact number of distinct members. When the set has
// spilled it first merges every run into one, which costs a pass over
// the on-disk keys.
func (s *HashSet) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.runs) == 0 {
		return len(s.mem)
	}
	if err := s.flushLocked(); err != nil {
		s.setErr(err)
		return len(s.mem)
	}
	if err := s.compactAllLocked(); err != nil {
		s.setErr(err)
	}
	var n int64
	for _, r := range s.runs {
		n += r.n
	}
	return int(n)
}

// ForEach calls fn for every member in ascending byte order, merging
// the runs and the heap buffer. Memory use is O(number of runs).
func (s *HashSet) ForEach(fn func(Key) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.runs) > 0 {
		if err := s.flushLocked(); err != nil {
			return err
		}
	}
	if len(s.runs) == 0 {
		keys := make([]Key, 0, len(s.mem))
		for k := range s.mem {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i][:], keys[j][:]) < 0 })
		for _, k := range keys {
			if err := fn(k); err != nil {
				return err
			}
		}
		return nil
	}
	return mergeRuns(s.runs, func(k Key) error { return fn(k) })
}

// Spilled reports how many run files back the set (0 = all on heap).
func (s *HashSet) Spilled() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.runs)
}

// Close removes every spill file. The set is empty afterwards. It
// returns the sticky spill error, if any, so a deferred Close does not
// hide a failure the caller never looked at.
func (s *HashSet) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.Err()
	}
	s.closed = true
	for _, r := range s.runs {
		r.close()
	}
	s.runs = nil
	s.mem = nil
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
	return s.Err()
}

func (s *HashSet) inRunsLocked(k Key) bool {
	for i := len(s.runs) - 1; i >= 0; i-- {
		found, err := s.runs[i].has(k)
		if err != nil {
			// A run that cannot be read cannot vouch for absence. Answer
			// "present" so a GC never deletes a chunk because of it, and
			// record the failure for callers that check Err.
			s.setErr(err)
			return true
		}
		if found {
			return true
		}
	}
	return false
}

func (s *HashSet) ensureDirLocked() error {
	if s.dir != "" {
		return nil
	}
	d, err := os.MkdirTemp(s.opts.Dir, "pghs-spill-*")
	if err != nil {
		return fmt.Errorf("spill: create temp dir: %w", err)
	}
	s.dir = d
	return nil
}

// flushLocked writes the heap buffer as a new level-0 run, then merges
// equal-level neighbours so the run count stays logarithmic.
func (s *HashSet) flushLocked() error {
	if len(s.mem) == 0 {
		return nil
	}
	if err := s.ensureDirLocked(); err != nil {
		return err
	}
	keys := make([]Key, 0, len(s.mem))
	for k := range s.mem {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i][:], keys[j][:]) < 0 })
	i := 0
	r, err := s.writeRunLocked(0, int64(len(keys)), func() (Key, bool, error) {
		if i == len(keys) {
			return Key{}, false, nil
		}
		k := keys[i]
		i++
		return k, true, nil
	})
	if err != nil {
		return err
	}
	s.runs = append(s.runs, r)
	s.mem = make(map[Key]struct{})
	for n := len(s.runs); n >= 2 && s.runs[n-1].level == s.runs[n-2].level; n = len(s.runs) {
		if err := s.mergeTailLocked(2); err != nil {
			return err
		}
	}
	return nil
}

func (s *HashSet) compactAllLocked() error {
	if len(s.runs) <= 1 {
		return nil
	}
	return s.mergeTailLocked(len(s.runs))
}

// mergeTailLocked merges the newest n runs into one.
func (s *HashSet) mergeTailLocked(n int) error {
	in := s.runs[len(s.runs)-n:]
	level, upper := 0, int64(0)
	for _, r := range in {
		if r.level >= level {
			level = r.level + 1
		}
		upper += r.n
	}
	next, stop := newMergeIter(in)
	defer stop()
	merged, err := s.writeRunLocked(level, upper, next)
	if err != nil {
		return err
	}
	for _, r := range in {
		r.close()
		_ = os.Remove(r.path)
	}
	s.runs = append(s.runs[:len(s.runs)-n], merged)
	return nil
}

// writeRunLocked drains next into a new sorted, deduplicated run file.
// upper is an upper bound on the key count, used to size the Bloom
// filter before the exact count is known.
func (s *HashSet) writeRunLocked(level int, upper int64, next func() (Key, bool, error)) (*run, error) {
	s.seq++
	path := fmt.Sprintf("%s/run-%06d", s.dir, s.seq)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("spill: create run: %w", err)
	}
	r := &run{f: f, path: path, level: level, bloom: newBloom(upper)}
	w := bufio.NewWriterSize(f, 1<<20)
	var prev Key
	for {
		k, ok, err := next()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if !ok {
			break
		}
		if r.n > 0 && k == prev {
			continue
		}
		prev = k
		if r.n%fenceEvery == 0 {
			r.fence = append(r.fence, k)
		}
		r.bloom.add(k)
		if _, err := w.Write(k[:]); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("spill: write run: %w", err)
		}
		r.n++
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("spill: flush run: %w", err)
	}
	return r, nil
}

// run is one immutable sorted file of keys plus its in-memory filters.
type run struct {
	f     *os.File
	path  string
	level int
	n     int64
	bloom *bloom
	fence []Key // fence[i] = key number i*fenceEvery
}

func (r *run) close() {
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
}

func (r *run) has(k Key) (bool, error) {
	if r.n == 0 || !r.bloom.test(k) {
		return false, nil
	}
	// Last fence entry <= k names the block that could hold k.
	b := sort.Search(len(r.fence), func(i int) bool { return bytes.Compare(r.fence[i][:], k[:]) > 0 }) - 1
	if b < 0 {
		return false, nil
	}
	first := int64(b) * fenceEvery
	cnt := r.n - first
	if cnt > fenceEvery {
		cnt = fenceEvery
	}
	buf := make([]byte, cnt*keyLen)
	if n, err := r.f.ReadAt(buf, first*keyLen); err != nil {
		// io.EOF with fewer bytes than the run's own key count means the
		// file was truncated behind our back: that is damage, not absence.
		if errors.Is(err, io.EOF) {
			err = fmt.Errorf("short read (%d of %d bytes): %w", n, len(buf), io.ErrUnexpectedEOF)
		}
		return false, fmt.Errorf("spill: read run %s: %w", r.path, err)
	}
	i := sort.Search(int(cnt), func(i int) bool { return bytes.Compare(buf[i*keyLen:(i+1)*keyLen], k[:]) >= 0 })
	return i < int(cnt) && bytes.Equal(buf[i*keyLen:(i+1)*keyLen], k[:]), nil
}

// ---- k-way merge ----

type mergeSrc struct {
	r   *bufio.Reader
	cur Key
}

type mergeHeap []*mergeSrc

func (h mergeHeap) Len() int           { return len(h) }
func (h mergeHeap) Less(i, j int) bool { return bytes.Compare(h[i].cur[:], h[j].cur[:]) < 0 }
func (h mergeHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *mergeHeap) Push(x any)        { *h = append(*h, x.(*mergeSrc)) }
func (h *mergeHeap) Pop() any          { o := *h; n := len(o); x := o[n-1]; *h = o[:n-1]; return x }

// newMergeIter returns a sorted (possibly duplicate-bearing) iterator
// over the given runs; stop releases its readers.
func newMergeIter(rs []*run) (next func() (Key, bool, error), stop func()) {
	h := &mergeHeap{}
	var initErr error
	for _, r := range rs {
		if r.n == 0 {
			continue
		}
		src := &mergeSrc{r: bufio.NewReaderSize(io.NewSectionReader(r.f, 0, r.n*keyLen), 1<<20)}
		if _, err := io.ReadFull(src.r, src.cur[:]); err != nil {
			initErr = err
			break
		}
		*h = append(*h, src)
	}
	heap.Init(h)
	next = func() (Key, bool, error) {
		if initErr != nil {
			return Key{}, false, fmt.Errorf("spill: merge: %w", initErr)
		}
		if h.Len() == 0 {
			return Key{}, false, nil
		}
		top := (*h)[0]
		out := top.cur
		if _, err := io.ReadFull(top.r, top.cur[:]); err != nil {
			if errors.Is(err, io.EOF) {
				heap.Pop(h)
			} else {
				return Key{}, false, fmt.Errorf("spill: merge read: %w", err)
			}
		} else {
			heap.Fix(h, 0)
		}
		return out, true, nil
	}
	return next, func() {}
}

func mergeRuns(rs []*run, fn func(Key) error) error {
	next, stop := newMergeIter(rs)
	defer stop()
	var prev Key
	first := true
	for {
		k, ok, err := next()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if !first && k == prev {
			continue
		}
		first, prev = false, k
		if err := fn(k); err != nil {
			return err
		}
	}
}
