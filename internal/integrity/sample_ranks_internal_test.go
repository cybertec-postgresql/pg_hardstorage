package integrity

import (
	"math/rand"
	"testing"
)

func draw(n int, s Strategy) []int {
	rng := func(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }
	sr := pickSampleRanks(n, s, rng)
	var out []int
	for i := 0; i < n; i++ {
		if sr.next() {
			out = append(out, i)
		}
	}
	return out
}

// The sampler holds O(1) state yet picks exactly the requested count,
// reproducibly for a seed.
func TestPickSampleRanks_ExactCountAndDeterministic(t *testing.T) {
	for _, c := range []struct {
		n, count int
		seed     int64
	}{{10, 3, 1}, {100, 17, 42}, {1000, 999, 7}, {5000, 250, 99}, {1, 1, 5}, {50, 1, 3}} {
		s := Strategy{Mode: "content-sample", Count: c.count, Seed: c.seed}
		a, b := draw(c.n, s), draw(c.n, s)
		if len(a) != c.count {
			t.Fatalf("n=%d count=%d: %d picks", c.n, c.count, len(a))
		}
		if len(a) != len(b) {
			t.Fatalf("not deterministic")
		}
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("n=%d seed=%d: run differs at %d", c.n, c.seed, i)
			}
		}
	}
}

func TestPickSampleRanks_PercentSeedsAndModes(t *testing.T) {
	if got := draw(1000, Strategy{Mode: "content-sample", Percent: 10, Seed: 1}); len(got) != 100 {
		t.Errorf("10%% of 1000 picked %d", len(got))
	}
	if got := draw(7, Strategy{Mode: "content-sample", Percent: 1, Seed: 1}); len(got) != 1 {
		t.Errorf("a percent that rounds to 0 must still pick 1, got %d", len(got))
	}
	x, y := draw(1000, Strategy{Mode: "content-sample", Count: 50, Seed: 1}), draw(1000, Strategy{Mode: "content-sample", Count: 50, Seed: 2})
	same := true
	for i := range x {
		same = same && x[i] == y[i]
	}
	if same {
		t.Error("different seeds picked identical samples")
	}
	if got := draw(20, Strategy{Mode: "content-sample", Count: 20}); len(got) != 20 {
		t.Errorf("count>=n must pick all, got %d", len(got))
	}
	if got := draw(20, Strategy{Mode: "content-full"}); len(got) != 20 {
		t.Errorf("content-full picked %d", len(got))
	}
	for _, m := range []string{"presence", "manifests-only"} {
		if got := draw(20, Strategy{Mode: m}); len(got) != 0 {
			t.Errorf("%s picked %d", m, len(got))
		}
	}
}

// Rough uniformity: over many seeds every rank is picked about equally often.
func TestPickSampleRanks_Uniform(t *testing.T) {
	const n, count, trials = 20, 5, 4000
	hits := make([]int, n)
	for s := int64(1); s <= trials; s++ {
		for _, i := range draw(n, Strategy{Mode: "content-sample", Count: count, Seed: s}) {
			hits[i]++
		}
	}
	want := trials * count / n
	for i, h := range hits {
		if h < want*8/10 || h > want*12/10 {
			t.Errorf("rank %d picked %d times, want about %d", i, h, want)
		}
	}
}
