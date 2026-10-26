package spill

import (
	"crypto/sha256"
	"encoding/binary"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
)

// vmHWMBytes reads the process's peak resident set size (Linux).
func vmHWMBytes(t testing.TB) uint64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skip("no /proc/self/status")
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(ln, "VmHWM:") {
			f := strings.Fields(ln)
			kb, _ := strconv.ParseUint(f[1], 10, 64)
			return kb << 10
		}
	}
	t.Skip("no VmHWM")
	return 0
}

// TestScale_HashSetAndSorterRSS proves the two spill structures keep the
// process's peak RSS bounded as the element count grows. Opt-in:
// PGHS_SCALE=1; PGHS_SCALE_CHUNKS sets the element count (default 20M). The
// soft memory limit stands in for a constrained container.
func TestScale_HashSetAndSorterRSS(t *testing.T) {
	if os.Getenv("PGHS_SCALE") != "1" {
		t.Skip("set PGHS_SCALE=1 to run the spill RSS proof")
	}
	n := 20_000_000
	if v := os.Getenv("PGHS_SCALE_CHUNKS"); v != "" {
		n, _ = strconv.Atoi(v)
	}
	debug.SetMemoryLimit(512 << 20)

	dir := t.TempDir()
	set := NewHashSet(Options{Dir: dir})
	var b [8]byte
	for i := 0; i < n; i++ {
		binary.BigEndian.PutUint64(b[:], uint64(i))
		set.Add(sha256.Sum256(b[:]))
	}
	if set.Len() != n {
		t.Fatalf("set has %d, want %d", set.Len(), n)
	}
	if err := set.Err(); err != nil {
		t.Fatal(err)
	}
	_ = set.Close()
	hs := vmHWMBytes(t)
	t.Logf("HashSet of %d keys: peak RSS %d MiB", n, hs>>20)

	srt := NewSorter(SorterOptions{Dir: dir})
	defer srt.Close()
	val := make([]byte, 40)
	for i := 0; i < n/2; i++ {
		binary.BigEndian.PutUint64(b[:], uint64(i)*2654435761)
		k := sha256.Sum256(b[:])
		if err := srt.Add(k[:], val); err != nil {
			t.Fatal(err)
		}
	}
	it, err := srt.Iter()
	if err != nil {
		t.Fatal(err)
	}
	cnt := 0
	for {
		_, _, ok, err := it.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		cnt++
	}
	it.Close()
	if cnt != n/2 {
		t.Fatalf("sorter yielded %d, want %d", cnt, n/2)
	}
	t.Logf("after Sorter of %d records: peak RSS %d MiB", cnt, vmHWMBytes(t)>>20)
	if got := vmHWMBytes(t); got > 1<<30 {
		t.Errorf("peak RSS %d MiB over the 1 GiB bound for %d elements", got>>20, n)
	}
}
