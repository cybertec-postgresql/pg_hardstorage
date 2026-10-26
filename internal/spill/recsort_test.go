package spill

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"testing"
)

func TestSorter_OrdersAcrossSpilledRuns(t *testing.T) {
	for _, mem := range []int{1 << 30, 4 << 10} { // in memory, then ~thousands of runs
		s := NewSorter(SorterOptions{MemBytes: mem, Dir: t.TempDir()})
		const n = 40000
		rng := rand.New(rand.NewSource(1))
		want := map[uint64]string{}
		for _, v := range rng.Perm(n) {
			var k [8]byte
			binary.BigEndian.PutUint64(k[:], uint64(v))
			val := fmt.Sprintf("val-%d", v)
			want[uint64(v)] = val
			if err := s.Add(k[:], []byte(val)); err != nil {
				t.Fatal(err)
			}
		}
		it, err := s.Iter()
		if err != nil {
			t.Fatal(err)
		}
		var prev []byte
		got := 0
		for {
			k, v, ok, err := it.Next()
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			if prev != nil && bytes.Compare(prev, k) >= 0 {
				t.Fatalf("mem=%d: out of order at %d", mem, got)
			}
			prev = append(prev[:0], k...)
			if want[binary.BigEndian.Uint64(k)] != string(v) {
				t.Fatalf("value mismatch for %x", k)
			}
			got++
		}
		it.Close()
		if got != n {
			t.Fatalf("mem=%d: got %d records, want %d", mem, got, n)
		}
		dir := s.dir
		_ = s.Close()
		if mem < 1<<20 {
			if _, err := os.Stat(dir); err == nil {
				t.Errorf("run dir %s not removed", dir)
			}
		}
	}
}
