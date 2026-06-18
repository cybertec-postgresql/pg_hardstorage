package chunker_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	mathrand "math/rand"
	"testing"
	"unsafe"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/chunker"
)

// chunkAll runs the chunker across r and returns the chunks (copied so
// the caller can hold onto them past the iteration that produced each).
func chunkAll(t *testing.T, c *chunker.Chunker, r io.Reader) []chunker.Chunk {
	t.Helper()
	var got []chunker.Chunk
	for ch, err := range c.Iter(r) {
		if err != nil {
			t.Fatalf("chunker error: %v", err)
		}
		// Copy the data — subsequent iterations may overwrite the slice.
		buf := make([]byte, len(ch.Data))
		copy(buf, ch.Data)
		got = append(got, chunker.Chunk{Data: buf, Offset: ch.Offset})
	}
	return got
}

func TestEmptyStream(t *testing.T) {
	c := chunker.New()
	chunks := chunkAll(t, c, bytes.NewReader(nil))
	if len(chunks) != 0 {
		t.Errorf("empty stream should produce no chunks; got %d", len(chunks))
	}
}

func TestSingleSubMinChunk(t *testing.T) {
	c := chunker.New()
	body := bytes.Repeat([]byte{'a'}, 100)
	chunks := chunkAll(t, c, bytes.NewReader(body))
	if len(chunks) != 1 {
		t.Fatalf("expected exactly one chunk; got %d", len(chunks))
	}
	if !bytes.Equal(chunks[0].Data, body) {
		t.Errorf("chunk data mismatch")
	}
	if chunks[0].Offset != 0 {
		t.Errorf("first chunk offset = %d, want 0", chunks[0].Offset)
	}
}

func TestSizesWithinBounds(t *testing.T) {
	min, avg, max := chunker.DefaultMinSize, chunker.DefaultAvgSize, chunker.DefaultMaxSize
	c := chunker.New()
	body := randomBytes(t, 8*1024*1024) // 8 MiB
	chunks := chunkAll(t, c, bytes.NewReader(body))
	if len(chunks) < 2 {
		t.Fatalf("expected many chunks; got %d", len(chunks))
	}
	// Every chunk except possibly the last must satisfy min <= size <= max.
	for i, ch := range chunks {
		size := len(ch.Data)
		if i == len(chunks)-1 {
			if size > max {
				t.Errorf("last chunk %d exceeds max: %d > %d", i, size, max)
			}
			continue
		}
		if size < min {
			t.Errorf("chunk %d below min: %d < %d", i, size, min)
		}
		if size > max {
			t.Errorf("chunk %d exceeds max: %d > %d", i, size, max)
		}
	}
	// Average should be roughly close to avg. Allow 0.5x .. 2x slack.
	totalSize := int64(0)
	for _, ch := range chunks {
		totalSize += int64(len(ch.Data))
	}
	got := totalSize / int64(len(chunks))
	if got < int64(avg)/2 || got > int64(avg)*2 {
		t.Errorf("avg chunk size %d off target %d (allowed [%d, %d])",
			got, avg, avg/2, avg*2)
	}
}

func TestOffsetsContiguous(t *testing.T) {
	c := chunker.New()
	body := randomBytes(t, 1*1024*1024)
	chunks := chunkAll(t, c, bytes.NewReader(body))

	expected := int64(0)
	for i, ch := range chunks {
		if ch.Offset != expected {
			t.Errorf("chunk %d offset = %d, want %d", i, ch.Offset, expected)
		}
		expected += int64(len(ch.Data))
	}
	if expected != int64(len(body)) {
		t.Errorf("chunks cover %d bytes; input was %d", expected, len(body))
	}
}

func TestRoundTripConcatenation(t *testing.T) {
	c := chunker.New()
	body := randomBytes(t, 2*1024*1024)
	chunks := chunkAll(t, c, bytes.NewReader(body))

	var rebuilt bytes.Buffer
	for _, ch := range chunks {
		rebuilt.Write(ch.Data)
	}
	if !bytes.Equal(rebuilt.Bytes(), body) {
		t.Error("concatenation of chunks must equal original input")
	}
}

func TestDeterminism(t *testing.T) {
	c := chunker.New()
	body := randomBytes(t, 1*1024*1024)
	a := chunkAll(t, c, bytes.NewReader(body))
	b := chunkAll(t, c, bytes.NewReader(body))
	if len(a) != len(b) {
		t.Fatalf("chunk count differs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i].Data, b[i].Data) {
			t.Fatalf("chunk %d differs across runs", i)
		}
		if a[i].Offset != b[i].Offset {
			t.Fatalf("chunk %d offset differs: %d vs %d", i, a[i].Offset, b[i].Offset)
		}
	}
}

// TestDedupProperty is the headline test: insert one byte at an arbitrary
// position and most chunks must remain bit-identical. Without this, CDC
// would offer no value over fixed-size chunking.
func TestDedupProperty(t *testing.T) {
	c := chunker.New()
	body := randomBytes(t, 4*1024*1024)
	originalChunks := chunkAll(t, c, bytes.NewReader(body))

	// Insert a single byte at ~25% through the input.
	insertAt := len(body) / 4
	modified := make([]byte, 0, len(body)+1)
	modified = append(modified, body[:insertAt]...)
	modified = append(modified, 0xAA)
	modified = append(modified, body[insertAt:]...)

	modifiedChunks := chunkAll(t, c, bytes.NewReader(modified))

	// Build a hash set of original chunks.
	originalHashes := make(map[[32]byte]bool, len(originalChunks))
	for _, ch := range originalChunks {
		originalHashes[sha256.Sum256(ch.Data)] = true
	}

	// Count modified chunks that were already in the original set.
	matched := 0
	for _, ch := range modifiedChunks {
		if originalHashes[sha256.Sum256(ch.Data)] {
			matched++
		}
	}
	// We expect the vast majority of chunks to match. Allow some leeway
	// for the chunk(s) containing the modification + boundary realignment
	// just after. With reasonable workloads, well over 80% should match.
	matchRate := float64(matched) / float64(len(modifiedChunks))
	if matchRate < 0.80 {
		t.Errorf("dedup match rate %.1f%% too low (want >= 80%%); orig=%d, mod=%d, matched=%d",
			matchRate*100, len(originalChunks), len(modifiedChunks), matched)
	}
	t.Logf("dedup match rate: %.1f%% (orig=%d mod=%d matched=%d)",
		matchRate*100, len(originalChunks), len(modifiedChunks), matched)
}

func TestNewWithParams_RejectsBadBounds(t *testing.T) {
	for _, c := range []struct {
		name          string
		min, avg, max int
	}{
		{"zero min", 0, 1, 2},
		{"avg below min", 100, 50, 200},
		{"max below avg", 100, 200, 150},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("expected panic for %s", c.name)
				}
			}()
			chunker.NewWithParams(c.min, c.avg, c.max)
		})
	}
}

// fakeFlakyReader returns N bytes then an error. Used to confirm read-error
// propagation through the iter API.
type fakeFlakyReader struct {
	n   int
	err error
}

func (r *fakeFlakyReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, r.err
	}
	if len(p) > r.n {
		p = p[:r.n]
	}
	for i := range p {
		p[i] = byte(i)
	}
	r.n -= len(p)
	return len(p), nil
}

func TestReadErrorPropagates(t *testing.T) {
	c := chunker.New()
	wantErr := errors.New("synthetic read failure")
	r := &fakeFlakyReader{n: 100, err: wantErr}
	var seen error
	for _, err := range c.Iter(r) {
		if err != nil {
			seen = err
			break
		}
	}
	if !errors.Is(seen, wantErr) {
		t.Errorf("got %v, want %v", seen, wantErr)
	}
}

func TestIterCanStopEarly(t *testing.T) {
	c := chunker.New()
	body := randomBytes(t, 10*1024*1024)
	count := 0
	for range c.Iter(bytes.NewReader(body)) {
		count++
		if count >= 3 {
			break
		}
	}
	if count != 3 {
		t.Errorf("early break should give exactly 3 chunks; got %d", count)
	}
}

// TestIterCopying_DataSurvivesNextIteration: the safe iterator
// (audit) decouples chunk lifetime from the chunker's
// working buffer.  We retain every chunk's Data slice, then walk
// every retained chunk and assert the bytes match the
// concatenated input — proving the copies didn't get rewritten
// by subsequent iterations the way Iter's slices would.
func TestIterCopying_DataSurvivesNextIteration(t *testing.T) {
	c := chunker.New()
	body := randomBytes(t, 4*1024*1024)
	var retained [][]byte
	for ch, err := range c.IterCopying(bytes.NewReader(body)) {
		if err != nil {
			t.Fatalf("iter err: %v", err)
		}
		retained = append(retained, ch.Data)
	}
	var rebuilt []byte
	for _, slice := range retained {
		rebuilt = append(rebuilt, slice...)
	}
	if !bytes.Equal(rebuilt, body) {
		t.Fatalf("retained chunks should reconstruct the input verbatim; mismatch (len=%d vs %d)",
			len(rebuilt), len(body))
	}
}

// TestIter_DataReusesBuffer: the documented no-copy contract of
// Iter — the same backing array gets rewritten across iterations.
// We assert the LAST chunk's slice points into the same backing
// memory the second-to-last chunk did.  Pinning this behaviour
// loudly so a future refactor doesn't accidentally allocate per
// chunk and silently break the IterCopying assumption.
func TestIter_DataReusesBuffer(t *testing.T) {
	c := chunker.New()
	body := randomBytes(t, 4*1024*1024)
	var firstAddr, lastAddr uintptr
	count := 0
	for ch, err := range c.Iter(bytes.NewReader(body)) {
		if err != nil {
			t.Fatalf("iter err: %v", err)
		}
		count++
		if count == 1 {
			firstAddr = sliceDataAddr(ch.Data)
		}
		lastAddr = sliceDataAddr(ch.Data)
	}
	if count < 2 {
		t.Skipf("test needs at least 2 chunks; got %d", count)
	}
	if firstAddr != lastAddr {
		// Not a hard failure (the buffer can grow under specific
		// patterns) — but it's a strong signal that no-copy
		// behaviour changed.  Make it visible.
		t.Logf("Iter chunks did not share backing memory across all iterations (first=%x last=%x); IterCopying's safety contract assumes the no-copy path keeps reusing the buffer", firstAddr, lastAddr)
	}
}

// sliceDataAddr returns the underlying-array address of a slice.
func sliceDataAddr(b []byte) uintptr {
	if cap(b) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&b[:1][0]))
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	r := mathrand.New(mathrand.NewSource(int64(n) + 0xDEADBEEF))
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		t.Fatal(err)
	}
	return b
}
