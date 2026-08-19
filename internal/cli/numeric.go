package cli

import "math"

// finiteFloat reports whether a float64 command-line option is a real,
// usable number — not NaN and not ±Inf. Go's flag parsing accepts "NaN",
// "Inf", and "+Inf" as valid float64s (strconv.ParseFloat does), so a
// numeric option can arrive non-finite and slip past a plain `x <= 0` /
// `x < 0` bound (every comparison with NaN is false, and Inf passes a
// lower bound), then feed a nonsensical multiplier or price into a gate.
// Options validate with `!finiteFloat(x) || <range>` so a non-finite value
// is rejected loudly as a usage error instead.
//
// Salvaged from PR #30 (postgresql007), the one piece of that
// now-superseded PR not already covered on main.
func finiteFloat(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
