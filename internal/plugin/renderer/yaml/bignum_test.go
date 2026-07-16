package yaml

import (
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

// Regression: jsonRoundTrip decoded every JSON number into float64, so
// yaml.v3 rendered byte counts like 331480761 as "3.31480761e+08" —
// unreadable, type-changing vs the JSON schema, and lossy past 2^53.
// Integral numbers must survive as plain integers.
func TestJSONRoundTrip_BigIntegersStayIntegers(t *testing.T) {
	in := struct {
		LogicalBytes int64   `json:"logical_bytes"`
		Huge         int64   `json:"huge"`
		Ratio        float64 `json:"ratio"`
	}{LogicalBytes: 331480761, Huge: 1 << 60, Ratio: 2.57}
	out, err := jsonRoundTrip(in)
	if err != nil {
		t.Fatalf("jsonRoundTrip: %v", err)
	}
	m := out.(map[string]any)
	if got, ok := m["logical_bytes"].(int64); !ok || got != 331480761 {
		t.Errorf("logical_bytes = %v (%T), want int64 331480761", m["logical_bytes"], m["logical_bytes"])
	}
	if got, ok := m["huge"].(int64); !ok || got != 1<<60 {
		t.Errorf("huge = %v (%T), want int64 2^60 (float64 would lose precision)", m["huge"], m["huge"])
	}
	if got, ok := m["ratio"].(float64); !ok || got != 2.57 {
		t.Errorf("ratio = %v (%T), want float64 2.57", m["ratio"], m["ratio"])
	}
}

// The rendered YAML text must contain the plain integer, no e-notation
// and no quotes.
func TestRender_NoScientificNotation(t *testing.T) {
	out, err := jsonRoundTrip(map[string]int64{"bytes": 331480761})
	if err != nil {
		t.Fatalf("jsonRoundTrip: %v", err)
	}
	s := marshalForTest(t, out)
	if strings.Contains(s, "e+") || strings.Contains(s, "E+") {
		t.Errorf("YAML contains scientific notation:\n%s", s)
	}
	if !strings.Contains(s, "331480761") || strings.Contains(s, `"331480761"`) {
		t.Errorf("YAML must contain the unquoted integer 331480761:\n%s", s)
	}
}

// marshalForTest renders v the way the renderer does.
func marshalForTest(t *testing.T, v any) string {
	t.Helper()
	b, err := yamlv3.Marshal(v)
	if err != nil {
		t.Fatalf("yaml marshal: %v", err)
	}
	return string(b)
}
