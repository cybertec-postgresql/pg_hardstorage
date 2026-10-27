package zstd

import (
	"math/rand"
	"runtime"
	"sync"
	"testing"
)

// The encoder pool must not scale with core count: a Compressor hit by far
// more goroutines than the cap keeps at most maxEncoderConcurrencyCap
// encoder states (each ~20 MiB) resident, and releases nothing it needs.
func TestCompressor_ResidentMemoryBoundedByEncoderCap(t *testing.T) {
	old := runtime.GOMAXPROCS(32)
	defer runtime.GOMAXPROCS(old)
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	c := NewDefault()
	in := make([]byte, 128<<10)
	rand.New(rand.NewSource(1)).Read(in)
	var wg sync.WaitGroup
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				out, _, err := c.Compress(in)
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := c.Decompress(out); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	grown := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	limit := int64(maxEncoderConcurrencyCap)*(24<<20) + (64 << 20)
	t.Logf("resident after 64-way compress: %d MiB (limit %d MiB)", grown>>20, limit>>20)
	if grown > limit {
		t.Fatalf("compressor retains %d MiB, over the %d MiB bound", grown>>20, limit>>20)
	}
	runtime.KeepAlive(c)
}
