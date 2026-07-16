package jsonshape

import "testing"

// Regression: a naive Unmarshal-into-any turned every JSON number into
// float64, so big byte counts rendered as scientific notation across
// yaml/csv/tap/junit/pdf/template output. Integral values must survive
// as int64 (including past 2^53), fractional as float64.
func TestRoundTrip_NumberFidelity(t *testing.T) {
	in := struct {
		Bytes int64   `json:"bytes"`
		Huge  int64   `json:"huge"`
		Ratio float64 `json:"ratio"`
		Rows  []int64 `json:"rows"`
	}{Bytes: 331480761, Huge: 1 << 60, Ratio: 2.57, Rows: []int64{9043645}}

	out, err := RoundTrip(in)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	m := out.(map[string]any)
	if got, ok := m["bytes"].(int64); !ok || got != 331480761 {
		t.Errorf("bytes = %v (%T), want int64", m["bytes"], m["bytes"])
	}
	if got, ok := m["huge"].(int64); !ok || got != 1<<60 {
		t.Errorf("huge = %v (%T), want exact int64 2^60", m["huge"], m["huge"])
	}
	if got, ok := m["ratio"].(float64); !ok || got != 2.57 {
		t.Errorf("ratio = %v (%T), want float64", m["ratio"], m["ratio"])
	}
	if got, ok := m["rows"].([]any)[0].(int64); !ok || got != 9043645 {
		t.Errorf("rows[0] = %v, want int64", m["rows"].([]any)[0])
	}
}
