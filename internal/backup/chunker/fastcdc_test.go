package chunker_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	mathrand "math/rand"
	"runtime"
	rtdebug "runtime/debug"
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
	var arrEnd uintptr
	count := 0
	for ch, err := range c.Iter(bytes.NewReader(body)) {
		if err != nil {
			t.Fatalf("iter err: %v", err)
		}
		if len(ch.Data) == 0 {
			continue
		}
		// Every chunk is a sub-slice of the single working buffer:
		// start + cap lands on the same backing-array end for all of
		// them. (The cursor design moves the live region to the front
		// periodically, so chunk START addresses are no longer equal —
		// the shared-array-end is the invariant.)
		end := uintptr(unsafe.Pointer(&ch.Data[0])) + uintptr(cap(ch.Data))
		if count == 0 {
			arrEnd = end
		} else if end != arrEnd {
			t.Fatalf("chunk %d does not share the working buffer's backing array (array end %x vs %x): Iter must reuse one buffer across iterations", count, end, arrEnd)
		}
		count++
	}
	if count < 2 {
		t.Skipf("test needs at least 2 chunks; got %d", count)
	}
}

// goldenBoundaries pins the chunk boundaries (offset + content hash)
// for a fixed 2 MiB input under the ORIGINAL tail-shift
// implementation. The perf-audit #7 cursor rewrite must be
// bit-identical to it: chunk boundaries feed straight into content
// hashes, so any drift silently regresses dedup against every chunk
// stored by an older binary. Regenerate ONLY if the boundary
// semantics intentionally change (a new chunker version).
var goldenBoundaries = []struct {
	off  int64
	hash string
}{
	{0, "1d9e9bdb488c6863b1c3de663407858b45d997648ff11fe244416a7ac9d66cd7"},
	{75004, "78cb81a31677a2721773493bc81a249e3e487d71ad8d8dce7e4f88f9e06e356d"},
	{161954, "89ac0c3391ffc99b9ab81818dbbcedb429f48042e9b46df4033cd062c720adc7"},
	{178927, "5865a9744a52ea5fc3147063ef4316903004ff96ef62983fa0456021dd4f65fb"},
	{222286, "203210efda1fa921d751644b9db6be25f283977a8d4722e4da21ce9f99097b2c"},
	{227426, "26288f0a60036e014d5a1605ffe66f8ed74d2e02ba6eb706515c69a0ad377c20"},
	{315903, "de0c86769d6b6233087878d2c107e5bf0cbc6752c1643c2f0981c14391ff7a63"},
	{384792, "604aac8a49ab0e242517c281c0e8bc2d90140d679c3f729fa562648f58e5d358"},
	{469812, "c0a9ebdb33fa7ada4703d822ddb8c37046b22a9188207c35b2b27d7a922495fb"},
	{575269, "d288bf9568c70aeb234585a51f866e8d0e34056a18f6ebc6db66b8545e0a6ba9"},
	{646482, "231b0e3a8cf1c1bf06f0b9c9b112ce44db6cba457129f0caf26c06efd2bb1970"},
	{736161, "437dfb5afb943e038a5c8c3ff3c5f2ea199c850a51f436c8ca177687aa89a018"},
	{795972, "df28b47d01550447382a2151371f8d2055c641b4644e934778ff6013f6aef855"},
	{877266, "3d2024771721887c2d86fdd523df460fbd29d6b3bb2ee92a24fbe75f92693777"},
	{909318, "c1f32c854c2f617a513cbe044fb07d2b40ecd206ce327c18b90dea689931834d"},
	{916650, "d3768e6a70a1d37ce8c0eee8de5557605a23e865de25b9ec484f061ece02780d"},
	{1007809, "ec87756f4de2636aebdbf46f0459984d378334f6eb540334644c5ea88f9cc14b"},
	{1074441, "3d7d8c2ee0a9c701e3b789bd326d86bf53edcdb3c0359686a0b8dedd61d7fd0f"},
	{1182642, "8a283f41f7edcb53e11e35752081a2beb2dd2b752d3d21150afff0a11f5b85b2"},
	{1274131, "5a9140f42fef38662413a2320c2845264cc087b41d4c39910c3f5d9f58c33aef"},
	{1352990, "5b5fec4187b9938f0b77cccec4989324a212b5ebb5b0f971e561d2bcfcf16df4"},
	{1434824, "1589e6aef4befb2a7953a1eda72483e29e99ea65b215c7cd190820574513ed5a"},
	{1535052, "5d51c352a8fd6356a3f86e236763418cd1175a41a781558f5a180b18c04bb2e3"},
	{1610345, "a83bbb5a9692e332a532e7b330986f88b81c90ef87970218ac9bd185af302ba1"},
	{1692970, "2bd7d1a997597a779ad491ca0201fe5bfbcdeed266b995649809888654f534a4"},
	{1769489, "9580566678216fe3699e30a0aca8dda64c51e119cb92ac8cba790e90341d7349"},
	{1849171, "88900bfe0284b533f6ef9819ba3743dab6a33ae1c06e01c7d51626d48dc6abff"},
	{1917171, "3e95d7eca04d05bd59e43dbfb10be6b252eb126deb9176ca523a77fcc7d82268"},
	{2008131, "21e186b770ea27e16a94c75c8329322a34a27daeef783f324ded00f850478c8f"},
	{2077607, "bce0bf2a4df485258f1066c808a9b37b0a0c59f8f8e6c422d49525a0404191f3"},
}

// TestIter_BoundariesGolden verifies the cursor rewrite produces
// exactly the same chunk boundaries (offsets + content hashes) as the
// original tail-shift implementation on a fixed input. This is the
// dedup contract: a chunk produced today must match a chunk produced
// by the pre-cursor code.
func TestIter_BoundariesGolden(t *testing.T) {
	src := mathrand.New(mathrand.NewSource(0x42))
	body := make([]byte, 2<<20)
	if _, err := io.ReadFull(src, body); err != nil {
		t.Fatal(err)
	}
	got := chunkAll(t, chunker.New(), bytes.NewReader(body))
	if len(got) != len(goldenBoundaries) {
		t.Fatalf("chunk count = %d, want %d — the cursor rewrite drifted the boundary count", len(got), len(goldenBoundaries))
	}
	for i, ch := range got {
		if ch.Offset != goldenBoundaries[i].off {
			t.Fatalf("chunk %d offset = %d, want %d — boundary drift vs the pre-cursor implementation", i, ch.Offset, goldenBoundaries[i].off)
		}
		sum := sha256.Sum256(ch.Data)
		if hex.EncodeToString(sum[:]) != goldenBoundaries[i].hash {
			t.Fatalf("chunk %d (off=%d) hash = %s, want %s — chunk content drifted", i, ch.Offset, hex.EncodeToString(sum[:]), goldenBoundaries[i].hash)
		}
	}
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

// TestIter_NoPerChunkAllocation pins the perf-audit #7 hot-path
// property: a full 4 MiB stream (~57 chunks at the default 64 KiB
// average) allocates a small constant — the single working buffer —
// not one allocation per chunk / per refill. The pre-cursor code
// measured ~280 allocations on the same stream (a fresh
// `tmp := make` per refill, plus the per-chunk variadic-argument
// boxing the guarded Asserts below now avoid).
//
// The GC is disabled for the window: GC background workers allocate
// (sudogs, goroutine stacks) and would pollute the count; restoring
// 100 is deferred. debug.SetGCPercent(-1) is the standard no-GC test
// protocol (runtime tests use the same).
func TestIter_NoPerChunkAllocation(t *testing.T) {
	rtdebug.SetGCPercent(-1)
	defer rtdebug.SetGCPercent(100)
	c := chunker.New()
	body := randomBytes(t, 4*1024*1024)
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	for range c.Iter(bytes.NewReader(body)) {
	}
	runtime.ReadMemStats(&m1)
	if allocs := m1.Mallocs - m0.Mallocs; allocs > 4 {
		t.Fatalf("Iter allocated %d objects over one 4 MiB stream; want a small constant (one working buffer) — per-chunk/per-refill allocation regression (perf audit #7)", allocs)
	}
}
