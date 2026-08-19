package cli

import (
	"math"
	"testing"
)

func TestFiniteFloat(t *testing.T) {
	finite := []float64{0, 1, -1, 1e300, -1e300, 0.023, 1.1}
	for _, v := range finite {
		if !finiteFloat(v) {
			t.Errorf("finiteFloat(%v) = false, want true", v)
		}
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if finiteFloat(v) {
			t.Errorf("finiteFloat(%v) = true, want false", v)
		}
	}
}
