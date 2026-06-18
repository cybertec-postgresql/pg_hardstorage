package agent

import (
	"testing"
	"time"
)

// TestJitteredInterval_StaysWithinBounds: the scaled interval is always
// within [base*(1-f), base*(1+f)] and the mean stays close to base
// (jitter is symmetric, so it doesn't change the average request rate).
func TestJitteredInterval_StaysWithinBounds(t *testing.T) {
	const base = 100 * time.Millisecond
	const f = 0.2
	lo := time.Duration(float64(base) * (1 - f))
	hi := time.Duration(float64(base) * (1 + f))

	var sum time.Duration
	const n = 5000
	for i := 0; i < n; i++ {
		d := jitteredInterval(base, f)
		if d < lo || d > hi {
			t.Fatalf("jitteredInterval = %v, outside [%v, %v]", d, lo, hi)
		}
		sum += d
	}
	mean := sum / n
	// Mean should be within 5% of base over 5000 samples.
	if mean < time.Duration(float64(base)*0.95) || mean > time.Duration(float64(base)*1.05) {
		t.Errorf("mean interval = %v, want ≈ %v", mean, base)
	}
}

// TestFirstInterval_SpreadsAcrossWholeInterval: the first tick is drawn
// from [0, base), so a fleet that started together decorrelates its
// first cycle.
func TestFirstInterval_SpreadsAcrossWholeInterval(t *testing.T) {
	const base = 100 * time.Millisecond
	const f = 0.2
	var seenLow, seenHigh bool
	for i := 0; i < 5000; i++ {
		d := firstInterval(base, f)
		if d < 0 || d >= base {
			t.Fatalf("firstInterval = %v, want [0, %v)", d, base)
		}
		if d < base/4 {
			seenLow = true
		}
		if d > base*3/4 {
			seenHigh = true
		}
	}
	if !seenLow || !seenHigh {
		t.Errorf("first-interval samples not spread across the range (low=%v high=%v)", seenLow, seenHigh)
	}
}

// TestJitter_DisabledIsExact: a non-positive fraction (the disabled
// state Run() produces from a negative JitterFraction) yields exact
// intervals — important for deterministic load tests.
func TestJitter_DisabledIsExact(t *testing.T) {
	const base = 100 * time.Millisecond
	for _, f := range []float64{0, -1} {
		if got := jitteredInterval(base, f); got != base {
			t.Errorf("jitteredInterval(%v, %v) = %v, want %v", base, f, got, base)
		}
		if got := firstInterval(base, f); got != base {
			t.Errorf("firstInterval(%v, %v) = %v, want %v", base, f, got, base)
		}
	}
}

// TestResolveJitterFraction: the default/disable convention — unset
// (0) turns jitter on by default; negative disables it.
func TestResolveJitterFraction(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{0, DefaultJitterFraction}, // unset → on by default
		{-1, 0},                    // negative → disabled
		{0.3, 0.3},                 // explicit → as-is
	}
	for _, tc := range cases {
		if got := resolveJitterFraction(tc.in); got != tc.want {
			t.Errorf("resolveJitterFraction(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestJitteredInterval_AlwaysPositive: even at the pathological
// fraction=1 edge, the scheduled interval is never non-positive.
func TestJitteredInterval_AlwaysPositive(t *testing.T) {
	const base = 50 * time.Millisecond
	for i := 0; i < 5000; i++ {
		if d := jitteredInterval(base, 1.0); d <= 0 {
			t.Fatalf("jitteredInterval produced a non-positive interval: %v", d)
		}
		// Over-large fractions are clamped, so still bounded above by 2x.
		if d := jitteredInterval(base, 5.0); d <= 0 || d > 2*base {
			t.Fatalf("jitteredInterval(over-large fraction) = %v, want (0, %v]", d, 2*base)
		}
	}
}
