package capacity

import "testing"

// Regression: a sub-day observation window must never yield medium/high
// confidence, no matter how tight R² — a per-day projection from
// seconds of data is meaningless (five backups in a minute previously
// gave "medium").
func TestConfidenceFor_ShortWindowCappedLow(t *testing.T) {
	// Perfect fit, plenty of samples, but only 90 seconds of window.
	if got := confidenceFor(0.99, 20, 90); got != "low" {
		t.Errorf("confidenceFor(tight fit, 90s window) = %q, want low", got)
	}
	// A week of samples with a good fit earns high.
	if got := confidenceFor(0.90, 12, 8*86400); got != "high" {
		t.Errorf("confidenceFor(week window) = %q, want high", got)
	}
	// A couple of days with a decent fit earns medium.
	if got := confidenceFor(0.75, 6, 3*86400); got != "medium" {
		t.Errorf("confidenceFor(3-day window) = %q, want medium", got)
	}
}
