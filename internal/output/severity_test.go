package output

import (
	"encoding/json"
	"testing"
)

func TestSeverity_String(t *testing.T) {
	cases := []struct {
		in   Severity
		want string
	}{
		{SeverityEmergency, "emergency"},
		{SeverityAlert, "alert"},
		{SeverityCritical, "critical"},
		{SeverityError, "error"},
		{SeverityWarning, "warning"},
		{SeverityNotice, "notice"},
		{SeverityInfo, "info"},
		{SeverityDebug, "debug"},
		{Severity(99), "severity(99)"},
		{Severity(-1), "severity(-1)"},
	}
	for _, c := range cases {
		if got := c.in.String(); got != c.want {
			t.Errorf("Severity(%d).String() = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSeverity_Valid(t *testing.T) {
	for s := SeverityEmergency; s <= SeverityDebug; s++ {
		if !s.Valid() {
			t.Errorf("Severity(%d) should be valid", s)
		}
	}
	for _, s := range []Severity{-1, 8, 99} {
		if s.Valid() {
			t.Errorf("Severity(%d) should be invalid", s)
		}
	}
}

func TestSeverity_AtLeast(t *testing.T) {
	cases := []struct {
		s, threshold Severity
		want         bool
	}{
		{SeverityError, SeverityWarning, true},   // error more severe than warning
		{SeverityWarning, SeverityError, false},  // warning less severe than error
		{SeverityWarning, SeverityWarning, true}, // equal counts
		{SeverityEmergency, SeverityDebug, true}, // most-severe vs least-severe
		{SeverityDebug, SeverityEmergency, false},
	}
	for _, c := range cases {
		if got := c.s.AtLeast(c.threshold); got != c.want {
			t.Errorf("%s.AtLeast(%s) = %v, want %v", c.s, c.threshold, got, c.want)
		}
	}
}

func TestParseSeverity(t *testing.T) {
	cases := []struct {
		in      string
		want    Severity
		wantErr bool
	}{
		{"info", SeverityInfo, false},
		{"INFO", SeverityInfo, false},
		{"  Warning  ", SeverityWarning, false},
		{"warn", SeverityWarning, false},
		{"err", SeverityError, false},
		{"crit", SeverityCritical, false},
		{"emerg", SeverityEmergency, false},
		{"informational", SeverityInfo, false},
		{"", 0, true},
		{"verbose", 0, true},
	}
	for _, c := range cases {
		got, err := ParseSeverity(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseSeverity(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("ParseSeverity(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestSeverity_TextRoundTrip(t *testing.T) {
	for s := SeverityEmergency; s <= SeverityDebug; s++ {
		b, err := s.MarshalText()
		if err != nil {
			t.Fatalf("marshal %s: %v", s, err)
		}
		var got Severity
		if err := got.UnmarshalText(b); err != nil {
			t.Fatalf("unmarshal %q: %v", b, err)
		}
		if got != s {
			t.Errorf("round-trip: got %s want %s", got, s)
		}
	}
}

func TestSeverity_MarshalText_Invalid(t *testing.T) {
	if _, err := Severity(99).MarshalText(); err == nil {
		t.Error("expected error marshaling invalid severity")
	}
}

func TestSeverity_JSON(t *testing.T) {
	type wrap struct {
		S Severity `json:"s"`
	}
	in := wrap{S: SeverityWarning}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"s":"warning"}`
	if string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
	var out wrap
	if err := json.Unmarshal([]byte(`{"s":"critical"}`), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.S != SeverityCritical {
		t.Errorf("got %s, want critical", out.S)
	}
}
