package spill

import (
	"bufio"
	"bytes"
	"container/heap"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
)

// Sorter is an external-memory sorter of (key, value) byte records. It
// exists for the places that need "join two big streams" or "order by a
// sequence number" over millions of records without holding them: compare
// of two manifests with 100M files, audit chain linkage over millions of
// events. Records are buffered up to MemBytes, sorted, and written as
// sorted run files; reading merges the runs, so resident memory is the
// buffer plus one small read buffer per run (merged in bounded fan-in
// passes), independent of the record count.
//
// Records with equal keys come out in an unspecified order. A Sorter is
// not safe for concurrent use.
type Sorter struct {
	memBytes int
	dir      string
	ownDir   bool

	arena []byte
	idx   []recRef
	runs  []string // sorted run files (full paths)
	seq   int
	n     int64
	err   error
	done  bool
}

type recRef struct{ off, klen, vlen int }

// SorterOptions configures a Sorter.
type SorterOptions struct {
	// MemBytes is the in-memory buffer before a run is flushed. Zero
	// means 16 MiB.
	MemBytes int
	// Dir is the parent directory for run files ("" = os.TempDir()). A
	// private subdirectory is created lazily and removed by Close.
	Dir string
}

const (
	defaultSorterMem = 16 << 20
	sorterFanIn      = 64
	sorterBufSize    = 64 << 10
	// maxRecordBytes bounds one record when reading a run back; it
	// catches a corrupt run instead of allocating its garbage length.
	maxRecordBytes = 1 << 30
)

// NewSorter returns an empty Sorter.
func NewSorter(opts SorterOptions) *Sorter {
	if opts.MemBytes <= 0 {
		opts.MemBytes = defaultSorterMem
	}
	return &Sorter{memBytes: opts.MemBytes, dir: opts.Dir}
}

// Len is the number of records added.
func (s *Sorter) Len() int64 { return s.n }

// Add appends a record. key and val are copied.
func (s *Sorter) Add(key, val []byte) error {
	if s.err != nil {
		return s.err
	}
	if s.done {
		return errors.New("spill: Sorter.Add after reading started")
	}
	s.idx = append(s.idx, recRef{off: len(s.arena), klen: len(key), vlen: len(val)})
	s.arena = append(s.arena, key...)
	s.arena = append(s.arena, val...)
	s.n++
	// The index costs ~24 bytes per record on top of the arena bytes.
	if len(s.arena)+len(s.idx)*24 >= s.memBytes {
		if err := s.flush(); err != nil {
			s.err = err
			return err
		}
	}
	return nil
}

func (s *Sorter) sortBuf() {
	sort.Slice(s.idx, func(i, j int) bool {
		a, b := s.idx[i], s.idx[j]
		return bytes.Compare(s.arena[a.off:a.off+a.klen], s.arena[b.off:b.off+b.klen]) < 0
	})
}

func (s *Sorter) ensureDir() error {
	if s.ownDir {
		return nil
	}
	d, err := os.MkdirTemp(s.dir, "pghs-sorter-*")
	if err != nil {
		return err
	}
	s.dir, s.ownDir = d, true
	return nil
}

func writeRec(w *bufio.Writer, key, val []byte) error {
	var hdr [2 * binary.MaxVarintLen64]byte
	n := binary.PutUvarint(hdr[:], uint64(len(key)))
	n += binary.PutUvarint(hdr[n:], uint64(len(val)))
	if _, err := w.Write(hdr[:n]); err != nil {
		return err
	}
	if _, err := w.Write(key); err != nil {
		return err
	}
	_, err := w.Write(val)
	return err
}

func (s *Sorter) newRunFile() (*os.File, string, error) {
	if err := s.ensureDir(); err != nil {
		return nil, "", err
	}
	s.seq++
	p := fmt.Sprintf("%s/run-%06d", s.dir, s.seq)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	return f, p, err
}

func (s *Sorter) flush() error {
	if len(s.idx) == 0 {
		return nil
	}
	s.sortBuf()
	f, p, err := s.newRunFile()
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, sorterBufSize)
	for _, r := range s.idx {
		if err = writeRec(w, s.arena[r.off:r.off+r.klen], s.arena[r.off+r.klen:r.off+r.klen+r.vlen]); err != nil {
			break
		}
	}
	if err == nil {
		err = w.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(p)
		return err
	}
	s.runs = append(s.runs, p)
	s.arena = s.arena[:0]
	s.idx = s.idx[:0]
	return nil
}

// runReader streams one sorted run.
type runReader struct {
	f    *os.File
	r    *bufio.Reader
	key  []byte
	val  []byte
	live bool
}

func openRun(p string) (*runReader, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	return &runReader{f: f, r: bufio.NewReaderSize(f, sorterBufSize)}, nil
}

// advance loads the next record into key/val; live=false at EOF.
func (rr *runReader) advance() error {
	kl, err := binary.ReadUvarint(rr.r)
	if err != nil {
		rr.live = false
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	vl, err := binary.ReadUvarint(rr.r)
	if err != nil {
		return fmt.Errorf("spill: truncated sorter run: %w", err)
	}
	if kl > maxRecordBytes || vl > maxRecordBytes {
		return errors.New("spill: corrupt sorter run (record length)")
	}
	if cap(rr.key) < int(kl) {
		rr.key = make([]byte, kl)
	}
	if cap(rr.val) < int(vl) {
		rr.val = make([]byte, vl)
	}
	rr.key, rr.val = rr.key[:kl], rr.val[:vl]
	if _, err := io.ReadFull(rr.r, rr.key); err != nil {
		return fmt.Errorf("spill: truncated sorter run: %w", err)
	}
	if _, err := io.ReadFull(rr.r, rr.val); err != nil {
		return fmt.Errorf("spill: truncated sorter run: %w", err)
	}
	rr.live = true
	return nil
}

type runHeap []*runReader

func (h runHeap) Len() int           { return len(h) }
func (h runHeap) Less(i, j int) bool { return bytes.Compare(h[i].key, h[j].key) < 0 }
func (h runHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *runHeap) Push(x any)        { *h = append(*h, x.(*runReader)) }
func (h *runHeap) Pop() any          { o := *h; n := len(o); x := o[n-1]; *h = o[:n-1]; return x }

// recMerge merges a set of runs.
type recMerge struct {
	all  []*runReader
	h    runHeap
	cur  *runReader // reader whose record was last returned; advanced lazily
	init bool
}

func newRecMerge(paths []string) (*recMerge, error) {
	m := &recMerge{}
	for _, p := range paths {
		rr, err := openRun(p)
		if err != nil {
			m.close()
			return nil, err
		}
		m.all = append(m.all, rr)
		if err := rr.advance(); err != nil {
			m.close()
			return nil, err
		}
		if rr.live {
			m.h = append(m.h, rr)
		}
	}
	heap.Init(&m.h)
	return m, nil
}

func (m *recMerge) next() (key, val []byte, ok bool, err error) {
	if m.cur != nil {
		if err := m.cur.advance(); err != nil {
			return nil, nil, false, err
		}
		if m.cur.live {
			heap.Push(&m.h, m.cur)
		}
		m.cur = nil
	}
	if len(m.h) == 0 {
		return nil, nil, false, nil
	}
	m.cur = heap.Pop(&m.h).(*runReader)
	return m.cur.key, m.cur.val, true, nil
}

func (m *recMerge) close() {
	for _, rr := range m.all {
		_ = rr.f.Close()
	}
	m.all = nil
}

// compact merges runs in passes of at most sorterFanIn files until that
// few remain, so the final read holds a bounded number of descriptors and
// buffers however many runs the input produced.
func (s *Sorter) compact() error {
	for len(s.runs) > sorterFanIn {
		var next []string
		for i := 0; i < len(s.runs); i += sorterFanIn {
			group := s.runs[i:min(i+sorterFanIn, len(s.runs))]
			if len(group) == 1 {
				next = append(next, group[0])
				continue
			}
			m, err := newRecMerge(group)
			if err != nil {
				return err
			}
			f, p, err := s.newRunFile()
			if err != nil {
				m.close()
				return err
			}
			w := bufio.NewWriterSize(f, sorterBufSize)
			for {
				k, v, ok, nerr := m.next()
				if nerr != nil {
					err = nerr
					break
				}
				if !ok {
					break
				}
				if err = writeRec(w, k, v); err != nil {
					break
				}
			}
			if err == nil {
				err = w.Flush()
			}
			m.close()
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				_ = os.Remove(p)
				return err
			}
			for _, g := range group {
				_ = os.Remove(g)
			}
			next = append(next, p)
		}
		s.runs = next
	}
	return nil
}

// Iter is a pull iterator over the sorted records.
type Iter struct {
	mem   *Sorter // set when nothing spilled
	i     int
	merge *recMerge
}

// Iter finishes adding and returns an iterator over the records in key
// order. The slices returned by Next are valid only until the following
// call to Next. Further Add calls fail.
func (s *Sorter) Iter() (*Iter, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.done = true
	if len(s.runs) == 0 {
		s.sortBuf()
		return &Iter{mem: s}, nil
	}
	if err := s.flush(); err != nil {
		s.err = err
		return nil, err
	}
	if err := s.compact(); err != nil {
		s.err = err
		return nil, err
	}
	m, err := newRecMerge(s.runs)
	if err != nil {
		s.err = err
		return nil, err
	}
	return &Iter{merge: m}, nil
}

// Next returns the next record; ok is false at the end.
func (it *Iter) Next() (key, val []byte, ok bool, err error) {
	if it.mem != nil {
		if it.i >= len(it.mem.idx) {
			return nil, nil, false, nil
		}
		r := it.mem.idx[it.i]
		it.i++
		a := it.mem.arena
		return a[r.off : r.off+r.klen], a[r.off+r.klen : r.off+r.klen+r.vlen], true, nil
	}
	return it.merge.next()
}

// Close releases the iterator's files (the Sorter's Close removes them).
func (it *Iter) Close() {
	if it.merge != nil {
		it.merge.close()
	}
}

// Close removes every run file and the private directory.
func (s *Sorter) Close() error {
	s.arena, s.idx = nil, nil
	if s.ownDir {
		err := os.RemoveAll(s.dir)
		s.ownDir = false
		s.runs = nil
		return err
	}
	return nil
}
