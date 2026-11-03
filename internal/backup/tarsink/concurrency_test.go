package tarsink_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/chunker"
	"io"
	"math/rand"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/tarsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/basebackup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// sinkWithConcurrency builds a Sink whose per-file chunk pool runs at
// the given concurrency.
func sinkWithConcurrency(t *testing.T, n int) (*tarsink.Sink, *repo.CAS) {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	cas := repo.NewCAS(sp)
	return tarsink.New(context.Background(), cas, tarsink.WithChunkConcurrency(n)), cas
}

// The per-file worker pool finishes chunks out of order; entry.Chunks
// must still come back in strict file-offset order, contiguous, and
// reconstruct byte-for-byte. Run across a range of concurrencies so
// the serial path (1) and a deep pool (16) are both covered.
func TestConcurrency_ChunkOrderAndReconstruction(t *testing.T) {
	// ~2 MiB of incompressible random data => many chunks, so the
	// pool genuinely runs out of order.
	body := make([]byte, 2<<20)
	rand.New(rand.NewSource(0x5EED)).Read(body)
	tarBytes := buildTar(t, []fileSpec{{name: "base/16384/99999", body: body}})

	for _, conc := range []int{1, 2, 8, 16} {
		t.Run("concurrency="+itoa(conc), func(t *testing.T) {
			sink, cas := sinkWithConcurrency(t, conc)
			if err := drive(t, sink, 0, basebackup.TablespaceInfo{OID: 1663}, tarBytes, 64*1024); err != nil {
				t.Fatalf("drive: %v", err)
			}
			files := sink.Files(0)
			if len(files) != 1 {
				t.Fatalf("Files(0) len = %d, want 1", len(files))
			}
			chunks := files[0].Chunks
			if len(chunks) < 4 {
				t.Fatalf("only %d chunks — too few to exercise the pool", len(chunks))
			}

			// Offsets must be strictly increasing AND contiguous:
			// chunk[i+1].Offset == chunk[i].Offset + chunk[i].Len.
			var want int64
			var rebuilt bytes.Buffer
			for i, ref := range chunks {
				if ref.Offset != want {
					t.Fatalf("chunk %d: Offset = %d, want %d (out-of-order reassembly)",
						i, ref.Offset, want)
				}
				want += ref.Len
				bs, err := cas.GetChunkBytes(context.Background(), ref.Hash)
				if err != nil {
					t.Fatalf("GetChunkBytes chunk %d: %v", i, err)
				}
				rebuilt.Write(bs)
			}
			if !bytes.Equal(rebuilt.Bytes(), body) {
				t.Errorf("reconstruction mismatch at concurrency %d", conc)
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// streamRec records the BeginFile/AddChunk/EndFile calls and the largest
// number of chunks any one file had outstanding in the sink (none: refs
// are handed over as they complete, never accumulated per file).
type streamRec struct {
	events []string
	chunks []backup.ChunkRef
	open   bool
}

func (r *streamRec) BeginFile(fe *backup.FileEntry) error {
	if r.open || len(fe.Chunks) != 0 {
		return errors.New("bad begin")
	}
	r.open = true
	r.events = append(r.events, "begin:"+fe.Path)
	return nil
}
func (r *streamRec) AddChunk(c backup.ChunkRef) error {
	if !r.open {
		return errors.New("chunk outside file")
	}
	r.chunks = append(r.chunks, c)
	return nil
}
func (r *streamRec) EndFile() error {
	r.open = false
	r.events = append(r.events, "end")
	return nil
}

// With WithEntryStream the sink emits chunk refs in file order as they
// complete, the entry list stays empty, and the refs equal what the
// accumulating path produces.
func TestEntryStream_InOrderAndEquivalent(t *testing.T) {
	body := make([]byte, 1<<20)
	rand.New(rand.NewSource(0xBEEF)).Read(body)
	tarBytes := buildTar(t, []fileSpec{
		{name: "base/1/a", body: body},
		{name: "base/1/empty", body: nil},
	})
	for _, conc := range []int{1, 3, 16} {
		sp := &fs.Plugin{}
		if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sp.Close() })
		rec := &streamRec{}
		small := func() *chunker.Chunker { return chunker.NewWithParams(1024, 4096, 16384) }
		sink := tarsink.New(context.Background(), repo.NewCAS(sp), tarsink.WithChunkConcurrency(conc),
			tarsink.WithChunkerFactory(small), tarsink.WithEntryStream(rec))
		if err := drive(t, sink, 0, basebackup.TablespaceInfo{OID: 1663}, tarBytes, 64*1024); err != nil {
			t.Fatalf("conc=%d drive: %v", conc, err)
		}
		if got := sink.Files(0); len(got) != 0 {
			t.Fatalf("conc=%d: sink retained %d entries", conc, len(got))
		}
		want := []string{"begin:base/1/a", "end", "begin:base/1/empty", "end"}
		if fmt.Sprint(rec.events) != fmt.Sprint(want) {
			t.Fatalf("conc=%d events = %v", conc, rec.events)
		}
		if len(rec.chunks) < 50 {
			t.Fatalf("conc=%d only %d chunks", conc, len(rec.chunks))
		}
		var off int64
		for i, c := range rec.chunks {
			if c.Offset != off {
				t.Fatalf("conc=%d chunk %d offset %d want %d", conc, i, c.Offset, off)
			}
			off += c.Len
		}
		if off != int64(len(body)) {
			t.Fatalf("conc=%d total %d", conc, off)
		}
	}
}

// slowPutSP delays every Put and records how many run at once — a
// stand-in for a remote object store, where a chunk write is a network
// round trip rather than CPU.
type slowPutSP struct {
	*fs.Plugin
	delay        time.Duration
	active, peak atomic.Int32
}

func (s *slowPutSP) Put(ctx context.Context, key string, r io.Reader, o storage.PutOptions) (storage.PutResult, error) {
	n := s.active.Add(1)
	defer s.active.Add(-1)
	for {
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(s.delay)
	return s.Plugin.Put(ctx, key, r, o)
}

// Issue #90: one-chunk files (most of PGDATA) used to upload one at a
// time, because each file waited for its last chunk before the next tar
// entry was read. The pool now spans files: many small files must have
// their uploads in flight together, and the per-file emission — stream
// events, chunk refs and observer callbacks — must stay in tar order.
func TestConcurrency_SmallFilesOverlapAcrossFiles(t *testing.T) {
	const (
		nFiles = 48
		conc   = 8
	)
	rng := rand.New(rand.NewSource(0x90))
	var specs []fileSpec
	for i := 0; i < nFiles; i++ {
		body := make([]byte, 2048)
		rng.Read(body)
		if i%7 == 3 {
			body = nil // empty files interleaved: end markers with no chunks
		}
		specs = append(specs, fileSpec{name: fmt.Sprintf("base/1/%d", 10000+i), body: body})
	}
	tarBytes := buildTar(t, specs)

	fsp := &fs.Plugin{}
	if err := fsp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsp.Close() })
	sp := &slowPutSP{Plugin: fsp, delay: 20 * time.Millisecond}
	cas := repo.NewCAS(sp)
	rec := &streamRec{}
	var observed []string
	sink := tarsink.New(context.Background(), cas,
		tarsink.WithChunkConcurrency(conc),
		tarsink.WithEntryStream(rec),
		tarsink.WithFileObserver(func(fs tarsink.FileStats) { observed = append(observed, fs.Path) }))

	start := time.Now()
	if err := drive(t, sink, 0, basebackup.TablespaceInfo{OID: 1663}, tarBytes, 64*1024); err != nil {
		t.Fatalf("drive: %v", err)
	}
	elapsed := time.Since(start)

	if p := sp.peak.Load(); p < conc/2 {
		t.Fatalf("peak concurrent Puts = %d, want >= %d: small files are not overlapping", p, conc/2)
	}
	// Serial would be ~nFiles × delay (~0.8 s); overlapped is a fraction.
	if serial := time.Duration(nFiles) * sp.delay; elapsed > serial/2 {
		t.Errorf("backup of %d one-chunk files took %v; serial would be %v", nFiles, elapsed, serial)
	}

	var want []string
	for _, f := range specs {
		want = append(want, "begin:"+f.name, "end")
	}
	if fmt.Sprint(rec.events) != fmt.Sprint(want) {
		t.Fatalf("stream events out of tar order:\n got %v\nwant %v", rec.events, want)
	}
	for i, f := range specs {
		if observed[i] != f.name {
			t.Fatalf("observer #%d = %s, want %s", i, observed[i], f.name)
		}
	}
	// Each non-empty file is one chunk, emitted in tar order.
	ci := 0
	for _, f := range specs {
		if len(f.body) == 0 {
			continue
		}
		bs, err := cas.GetChunkBytes(context.Background(), rec.chunks[ci].Hash)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bs, f.body) {
			t.Fatalf("chunk %d does not belong to %s", ci, f.name)
		}
		ci++
	}
	if ci != len(rec.chunks) {
		t.Fatalf("%d chunks emitted, want %d", len(rec.chunks), ci)
	}
}

// A BASE_BACKUP that fails mid-stream never reaches OnTablespaceEnd, so
// the parser and its uploads outlive the basebackup call. Close must
// not return while any PutChunk is still running: the runner tears the
// CAS down right after, and a late adoption into its closed set
// panicked a cancelled backup.
func TestClose_WaitsForInFlightUploads(t *testing.T) {
	rng := rand.New(rand.NewSource(0xC105E))
	var specs []fileSpec
	for i := 0; i < 64; i++ {
		body := make([]byte, 4096)
		rng.Read(body)
		specs = append(specs, fileSpec{name: fmt.Sprintf("base/1/%d", 20000+i), body: body})
	}
	tarBytes := buildTar(t, specs)

	fsp := &fs.Plugin{}
	if err := fsp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsp.Close() })
	sp := &slowPutSP{Plugin: fsp, delay: 30 * time.Millisecond}
	sink := tarsink.New(context.Background(), repo.NewCAS(sp), tarsink.WithChunkConcurrency(8))

	if err := sink.OnTablespaceStart(0, basebackup.TablespaceInfo{OID: 1663}); err != nil {
		t.Fatal(err)
	}
	// Half the stream, then abandon it — no OnTablespaceEnd.
	if err := sink.OnTablespaceData(0, tarBytes[:len(tarBytes)/2]); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for sp.active.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if sp.active.Load() == 0 {
		t.Fatal("no upload in flight to exercise Close against")
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if n := sp.active.Load(); n != 0 {
		t.Fatalf("Close returned with %d PutChunk calls still running", n)
	}
	if err := sink.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
}
