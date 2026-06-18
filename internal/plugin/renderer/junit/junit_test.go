package junit_test

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/junit"
)

// renderResult is a small helper that renders a Result and returns
// the XML string + the parsed structure for assertion.
func renderResult(t *testing.T, res *output.Result) (string, junit.TestSuites) {
	t.Helper()
	var buf bytes.Buffer
	if err := junit.New().RenderResult(&buf, res); err != nil {
		t.Fatalf("RenderResult: %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, xml.Header) {
		t.Errorf("output missing XML prolog: %q", out[:min(120, len(out))])
	}
	var parsed junit.TestSuites
	if err := xml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("XML parse: %v\n%s", err, out)
	}
	return out, parsed
}

func TestJUnit_NameAndSupportsTTY(t *testing.T) {
	r := junit.New()
	if r.Name() != "junit" {
		t.Errorf("Name = %q", r.Name())
	}
	if r.SupportsTTY() {
		t.Errorf("SupportsTTY should be false")
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestJUnit_EmptyResult_OnePassingCase(t *testing.T) {
	_, parsed := renderResult(t, output.NewResult("foo"))
	if parsed.Tests != 1 {
		t.Errorf("Tests = %d, want 1", parsed.Tests)
	}
	if parsed.Failures != 0 {
		t.Errorf("Failures = %d, want 0", parsed.Failures)
	}
	if len(parsed.Suites) != 1 || len(parsed.Suites[0].Cases) != 1 {
		t.Fatalf("shape: %+v", parsed)
	}
	if parsed.Suites[0].Cases[0].Failure != nil {
		t.Errorf("expected no failure")
	}
}

func TestJUnit_StructuredError_OneFailureCase(t *testing.T) {
	res := output.NewResult("foo").WithError(
		output.NewError("foo.bar", "boom").
			WithSuggestion(&output.Suggestion{Human: "do this"}),
	)
	_, parsed := renderResult(t, res)
	if parsed.Failures != 1 {
		t.Errorf("Failures = %d, want 1", parsed.Failures)
	}
	tc := parsed.Suites[0].Cases[0]
	if tc.Failure == nil {
		t.Fatal("expected failure block")
	}
	if !strings.Contains(tc.Failure.Message, "foo.bar") {
		t.Errorf("Message: %q", tc.Failure.Message)
	}
	if tc.Failure.Type != "foo.bar" {
		t.Errorf("Type: %q", tc.Failure.Type)
	}
	if !strings.Contains(tc.Failure.Body, "boom") {
		t.Errorf("Body missing boom: %q", tc.Failure.Body)
	}
	if !strings.Contains(tc.Failure.Body, "do this") {
		t.Errorf("Body missing suggestion: %q", tc.Failure.Body)
	}
}

func TestJUnit_QuorumNotMet_DrivesFailure(t *testing.T) {
	res := output.NewResult("threshold attest verify").WithBody(map[string]any{
		"met":              false,
		"threshold":        2,
		"valid_signatures": 1,
	})
	_, parsed := renderResult(t, res)
	if parsed.Failures != 1 {
		t.Errorf("Failures = %d, want 1", parsed.Failures)
	}
	if parsed.Suites[0].Cases[0].Name != "summary-quorum-not-met" {
		t.Errorf("name: %q", parsed.Suites[0].Cases[0].Name)
	}
}

func TestJUnit_QuorumMet_AllPass(t *testing.T) {
	res := output.NewResult("threshold attest verify").WithBody(map[string]any{
		"met":              true,
		"threshold":        2,
		"valid_signatures": 2,
	})
	_, parsed := renderResult(t, res)
	if parsed.Failures != 0 {
		t.Errorf("Failures = %d, want 0", parsed.Failures)
	}
}

func TestJUnit_StatusFoundIssues_DrivesFailure(t *testing.T) {
	res := output.NewResult("integrity verify").WithBody(map[string]any{
		"status": "found_issues",
	})
	_, parsed := renderResult(t, res)
	if parsed.Failures < 1 {
		t.Errorf("Failures = %d, want ≥ 1", parsed.Failures)
	}
}

func TestJUnit_FailuresSlice_OneCaseEach(t *testing.T) {
	res := output.NewResult("integrity run").WithBody(map[string]any{
		"status": "found_issues",
		"failures": []any{
			map[string]any{"reason": "missing", "chunk_hash": "deadbeef0001"},
			map[string]any{"reason": "hash_mismatch", "chunk_hash": "deadbeef0002"},
		},
	})
	_, parsed := renderResult(t, res)
	// Summary + 2 failures = 3 cases.
	if parsed.Tests != 3 {
		t.Errorf("Tests = %d, want 3", parsed.Tests)
	}
	if parsed.Failures != 3 {
		t.Errorf("Failures = %d, want 3 (summary + 2)", parsed.Failures)
	}
	// Per-failure cases should have hash-prefixed names.
	hasHash := false
	for _, c := range parsed.Suites[0].Cases {
		if strings.HasPrefix(c.Name, "chunk-deadbeef") {
			hasHash = true
		}
	}
	if !hasHash {
		t.Errorf("expected chunk-prefixed case name in:\n%+v", parsed.Suites[0].Cases)
	}
}

func TestJUnit_Findings_NoticeIsNotFailure(t *testing.T) {
	res := output.NewResult("insider scan").WithBody(map[string]any{
		"findings": []any{
			map[string]any{"type": "post_jit_destructive", "severity": "notice",
				"actor": "alice", "reason": "break-glass"},
		},
	})
	_, parsed := renderResult(t, res)
	if parsed.Failures != 0 {
		t.Errorf("Failures = %d, want 0 (notice ≠ failure)", parsed.Failures)
	}
	// Total tests: 1 summary + 1 finding.
	if parsed.Tests != 2 {
		t.Errorf("Tests = %d, want 2", parsed.Tests)
	}
}

func TestJUnit_Findings_WarningIsFailure(t *testing.T) {
	res := output.NewResult("insider scan").WithBody(map[string]any{
		"findings": []any{
			map[string]any{"type": "novel_principal", "severity": "warning",
				"actor": "bob", "reason": "new actor"},
		},
	})
	_, parsed := renderResult(t, res)
	if parsed.Failures < 1 {
		t.Errorf("Failures = %d, want ≥ 1", parsed.Failures)
	}
	// The actor name is in the testcase name (sanitised — `bob/novel_principal`).
	matched := false
	for _, c := range parsed.Suites[0].Cases {
		if strings.Contains(c.Name, "novel_principal") {
			matched = true
		}
	}
	if !matched {
		t.Errorf("expected novel_principal finding case:\n%+v", parsed.Suites[0].Cases)
	}
}

func TestJUnit_RenderEvent_FailureSeverity(t *testing.T) {
	ev := &output.Event{
		Component:    "wal.stream",
		Op:           "lag_alert",
		SeverityName: "error",
		Body:         map[string]any{"lag_seconds": 120},
	}
	var buf bytes.Buffer
	if err := junit.New().RenderEvent(&buf, ev); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	var parsed junit.TestSuites
	if err := xml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if parsed.Failures != 1 {
		t.Errorf("Failures = %d, want 1", parsed.Failures)
	}
}

func TestJUnit_RenderEvent_InfoSeverity_NoFailure(t *testing.T) {
	ev := &output.Event{
		Component:    "backup",
		Op:           "started",
		SeverityName: "info",
	}
	var buf bytes.Buffer
	if err := junit.New().RenderEvent(&buf, ev); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	var parsed junit.TestSuites
	if err := xml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if parsed.Failures != 0 {
		t.Errorf("Failures = %d, want 0", parsed.Failures)
	}
}

// TestJUnit_XML_IsParseable: every body shape must produce a
// well-formed XML document the standard library parses.
func TestJUnit_XML_IsParseable(t *testing.T) {
	cases := []*output.Result{
		output.NewResult("a"),
		output.NewResult("b").WithError(output.NewError("foo.bar", "x")),
		output.NewResult("c").WithBody(map[string]any{"met": true}),
		output.NewResult("d").WithBody(map[string]any{"met": false}),
		output.NewResult("e").WithBody([]any{}),
	}
	r := junit.New()
	for i, res := range cases {
		var buf bytes.Buffer
		if err := r.RenderResult(&buf, res); err != nil {
			t.Errorf("case %d: %v", i, err)
		}
		var parsed junit.TestSuites
		if err := xml.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Errorf("case %d not parseable: %v\n%s", i, err, buf.String())
		}
	}
}

// TestJUnit_Suite_HasTimestamp asserts the testsuite element carries
// an RFC3339 timestamp (CI dashboards index by this).
func TestJUnit_Suite_HasTimestamp(t *testing.T) {
	out, _ := renderResult(t, output.NewResult("foo"))
	if !strings.Contains(out, `timestamp="`) {
		t.Errorf("missing timestamp attribute in:\n%s", out)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
