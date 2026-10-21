package integrity

import (
	"math/rand"
	"testing"
)

// The sparse shuffle must pick exactly what the dense Fisher-Yates it
// replaced picked, so a recorded --seed keeps selecting the same chunks.
func TestPickSampleRanks_MatchesDenseShuffle(t *testing.T) {
	rng := func(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }
	for _, c := range []struct {
		n, count int
		seed     int64
	}{{10, 3, 1}, {100, 17, 42}, {1000, 999, 7}, {5000, 250, 99}, {1, 1, 5}} {
		idx := make([]int, c.n)
		for i := range idx {
			idx[i] = i
		}
		r := rng(c.seed)
		want := map[int64]struct{}{}
		for i := 0; i < c.count && c.count < c.n; i++ {
			j := i + r.Intn(len(idx)-i)
			idx[i], idx[j] = idx[j], idx[i]
		}
		for i := 0; i < c.count && c.count < c.n; i++ {
			want[int64(idx[i])] = struct{}{}
		}
		got := pickSampleRanks(c.n, Strategy{Mode: "content-sample", Count: c.count, Seed: c.seed}, rng)
		if c.count >= c.n {
			if !got.all {
				t.Errorf("n=%d count=%d: want all", c.n, c.count)
			}
			continue
		}
		if len(got.chosen) != len(want) {
			t.Fatalf("n=%d count=%d: %d picks, want %d", c.n, c.count, len(got.chosen), len(want))
		}
		for k := range want {
			if _, ok := got.chosen[k]; !ok {
				t.Fatalf("n=%d count=%d seed=%d: rank %d picked by dense shuffle but not by sparse", c.n, c.count, c.seed, k)
			}
		}
	}
}
