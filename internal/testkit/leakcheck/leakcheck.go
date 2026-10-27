// Package leakcheck is a small, dependency-free toolkit for tests that
// prove a long-running code path does not leak: file descriptors,
// memory mappings, goroutines, temp files, or heap that keeps growing
// with the number of iterations.
//
// Typical use:
//
//	base := leakcheck.Take()
//	for i := 0; i < n; i++ { doOneCycle() }
//	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{})
//
// and, for growth that must plateau rather than merely return to the
// baseline, Plateau samples the live heap across iterations.
package leakcheck

import (
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"testing"
	"time"
)

// Snapshot is a point-in-time resource census of the current process.
type Snapshot struct {
	FDs        int      // entries in /proc/self/fd (excluding the one used to list it)
	FDTargets  []string // readlink of each fd, for diffs
	Maps       int      // lines in /proc/self/maps
	MapsFiles  int      // file-backed mappings of regular paths under a temp dir
	Goroutines int
	HeapInuse  uint64 // bytes, after a forced GC
	HeapAlloc  uint64
	Stacks     map[string]int // goroutine count by top-of-stack function
}

// Tolerance relaxes the zero-growth assertion for counts that legitimately
// float (the Go runtime and net/http keep a few helper goroutines).
type Tolerance struct {
	FDs        int
	Goroutines int
	MapsFiles  int
}

// Take forces a GC (twice, so finalizers run) and records a census.
func Take() Snapshot {
	for i := 0; i < 2; i++ {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s := Snapshot{
		Goroutines: runtime.NumGoroutine(),
		HeapInuse:  ms.HeapInuse,
		HeapAlloc:  ms.HeapAlloc,
		Stacks:     goroutineTops(),
	}
	s.FDs, s.FDTargets = listFDs()
	s.Maps, s.MapsFiles = countMaps()
	return s
}

func listFDs() (int, []string) {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1, nil
	}
	var targets []string
	for _, e := range ents {
		t, err := os.Readlink("/proc/self/fd/" + e.Name())
		if err != nil {
			continue // the dir fd itself, or a racing close
		}
		targets = append(targets, t)
	}
	sort.Strings(targets)
	return len(targets), targets
}

func countMaps() (total, files int) {
	b, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		return -1, -1
	}
	for _, ln := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		total++
		f := strings.Fields(ln)
		if len(f) >= 6 && strings.HasPrefix(f[5], "/") && !strings.HasPrefix(f[5], "/usr/") &&
			!strings.HasPrefix(f[5], "/lib") && !strings.HasPrefix(f[5], "/proc/") && !strings.HasPrefix(f[5], "/dev/") &&
			(strings.Contains(f[5], "pghs-") || strings.Contains(f[5], "/tmp") || strings.Contains(f[5], "Test")) {
			files++
		}
	}
	return
}

func goroutineTops() map[string]int {
	p := pprof.Lookup("goroutine")
	var sb strings.Builder
	_ = p.WriteTo(&sb, 1)
	out := map[string]int{}
	// Format: "N @ 0x... \n#\t0x...\tfunc+0x..\tfile:line\n..."
	blocks := strings.Split(sb.String(), "\n\n")
	for _, b := range blocks {
		lines := strings.Split(strings.TrimSpace(b), "\n")
		if len(lines) < 2 || !strings.Contains(lines[0], " @ ") {
			continue
		}
		var n int
		fmt.Sscanf(lines[0], "%d", &n)
		top := strings.TrimSpace(lines[1])
		if f := strings.Fields(top); len(f) >= 3 {
			top = f[2]
		}
		out[top] += n
	}
	return out
}

// AssertNoLeak waits briefly for stragglers to exit, then fails t if any
// tracked resource grew past base by more than tol, printing what grew.
func AssertNoLeak(t testing.TB, base Snapshot, tol Tolerance) {
	t.Helper()
	var cur Snapshot
	deadline := time.Now().Add(5 * time.Second)
	for {
		cur = Take()
		if cur.Goroutines <= base.Goroutines+tol.Goroutines && cur.FDs <= base.FDs+tol.FDs && cur.MapsFiles <= base.MapsFiles+tol.MapsFiles {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if cur.FDs > base.FDs+tol.FDs {
		t.Errorf("fd leak: %d -> %d; new targets: %v", base.FDs, cur.FDs, diffStrings(base.FDTargets, cur.FDTargets))
	}
	if cur.MapsFiles > base.MapsFiles+tol.MapsFiles {
		t.Errorf("mmap leak: file-backed mappings %d -> %d", base.MapsFiles, cur.MapsFiles)
	}
	if cur.Goroutines > base.Goroutines+tol.Goroutines {
		var grown []string
		for k, v := range cur.Stacks {
			if v > base.Stacks[k] {
				grown = append(grown, fmt.Sprintf("%s +%d", k, v-base.Stacks[k]))
			}
		}
		sort.Strings(grown)
		t.Errorf("goroutine leak: %d -> %d; grown: %s", base.Goroutines, cur.Goroutines, strings.Join(grown, "; "))
	}
}

func diffStrings(a, b []string) []string {
	cnt := map[string]int{}
	for _, s := range a {
		cnt[s]++
	}
	var out []string
	for _, s := range b {
		if cnt[s] > 0 {
			cnt[s]--
			continue
		}
		out = append(out, s)
	}
	return out
}

// LiveHeap returns HeapAlloc after a forced GC.
func LiveHeap() uint64 {
	for i := 0; i < 2; i++ {
		runtime.GC()
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// Plateau runs step n times, sampling the live heap every `every`
// iterations, and fails if the heap in the last quarter of the samples
// exceeds the heap at the end of the warm-up (first quarter) by more than
// slack bytes. A linear leak of even a few hundred bytes per iteration
// shows up once n is large; a bounded cache does not.
func Plateau(t testing.TB, n, every int, slack uint64, step func(i int)) {
	t.Helper()
	var samples []uint64
	for i := 0; i < n; i++ {
		step(i)
		if (i+1)%every == 0 {
			samples = append(samples, LiveHeap())
			dumpHeap(t, len(samples))
		}
	}
	if len(samples) < 8 {
		t.Fatalf("Plateau needs at least 8 samples, got %d", len(samples))
	}
	q := len(samples) / 4
	var warm uint64
	for _, s := range samples[:q] {
		if s > warm {
			warm = s
		}
	}
	var tail uint64
	for _, s := range samples[len(samples)-q:] {
		if s > tail {
			tail = s
		}
	}
	t.Logf("heap plateau: warm-up max %d KiB, tail max %d KiB (slack %d KiB); samples(KiB)=%v",
		warm>>10, tail>>10, slack>>10, kib(samples))
	if tail > warm+slack {
		t.Errorf("heap keeps growing: warm-up max %d KiB -> tail max %d KiB (slack %d KiB)", warm>>10, tail>>10, slack>>10)
	}
}

func kib(s []uint64) []uint64 {
	o := make([]uint64, len(s))
	for i, v := range s {
		o[i] = v >> 10
	}
	return o
}

// DirEntries returns the names under dir (empty if it does not exist).
func DirEntries(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// dumpHeap writes an inuse heap profile for sample number k to
// $PGHS_HEAPPROF/<test>.<k>.heap when that variable is set, so two samples
// can be compared with `go tool pprof -base first last`.
func dumpHeap(t testing.TB, k int) {
	dir := os.Getenv("PGHS_HEAPPROF")
	if dir == "" {
		return
	}
	name := strings.ReplaceAll(t.Name(), "/", "_")
	f, err := os.Create(fmt.Sprintf("%s/%s.%03d.heap", dir, name, k))
	if err != nil {
		return
	}
	defer f.Close()
	_ = pprof.Lookup("heap").WriteTo(f, 0)
}
