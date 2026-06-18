package tap_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/tap"
)

// renderResult is a small helper that runs the TAP renderer against
// a given Result and returns the rendered string.
func renderResult(t *testing.T, res *output.Result) string {
	t.Helper()
	var buf bytes.Buffer
	if err := tap.New().RenderResult(&buf, res); err != nil {
		t.Fatalf("RenderResult: %v", err)
	}
	return buf.String()
}

func TestTAP_Header_IsVersion14(t *testing.T) {
	out := renderResult(t, output.NewResult("foo"))
	if !strings.HasPrefix(out, "TAP version 14\n") {
		t.Errorf("header: %q", firstLine(out))
	}
}

func TestTAP_EmptyBody_OneOK(t *testing.T) {
	out := renderResult(t, output.NewResult("foo"))
	want := "1..1\nok 1 - foo\n"
	if !strings.Contains(out, want) {
		t.Errorf("missing %q in:\n%s", want, out)
	}
}

func TestTAP_StructuredError_NotOK(t *testing.T) {
	res := output.NewResult("foo").WithError(
		output.NewError("foo.bar", "boom").
			WithSuggestion(&output.Suggestion{Human: "do this", Command: "pg_hardstorage retry"}),
	)
	out := renderResult(t, res)
	for _, want := range []string{
		"1..1",
		"not ok 1 - foo",
		"  ---",
		"code: foo.bar",
		"message: boom",
		"  ...",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestTAP_QuorumNotMet_BodyDrivesFailure(t *testing.T) {
	res := output.NewResult("threshold attest verify").WithBody(map[string]any{
		"met":              false,
		"threshold":        2,
		"valid_signatures": 1,
	})
	out := renderResult(t, res)
	if !strings.Contains(out, "not ok 1") {
		t.Errorf("expected not-ok summary, got:\n%s", out)
	}
	if !strings.Contains(out, "quorum not met") {
		t.Errorf("expected verdict text, got:\n%s", out)
	}
}

func TestTAP_QuorumMet_BodyDrivesPass(t *testing.T) {
	res := output.NewResult("threshold attest verify").WithBody(map[string]any{
		"met":              true,
		"threshold":        2,
		"valid_signatures": 2,
	})
	out := renderResult(t, res)
	if !strings.Contains(out, "ok 1") || strings.Contains(out, "not ok") {
		t.Errorf("expected ok 1, got:\n%s", out)
	}
}

func TestTAP_StatusFoundIssues_DrivesFailure(t *testing.T) {
	res := output.NewResult("integrity verify").WithBody(map[string]any{
		"status":          "found_issues",
		"signatures_fail": 2,
	})
	out := renderResult(t, res)
	if !strings.Contains(out, "not ok 1") {
		t.Errorf("expected not-ok, got:\n%s", out)
	}
}

func TestTAP_FailuresSlice_OneTestPointEach(t *testing.T) {
	res := output.NewResult("integrity run").WithBody(map[string]any{
		"status": "found_issues",
		"failures": []any{
			map[string]any{"reason": "missing", "chunk_hash": "deadbeef0001"},
			map[string]any{"reason": "hash_mismatch", "chunk_hash": "deadbeef0002"},
		},
	})
	out := renderResult(t, res)
	if !strings.Contains(out, "1..3") {
		t.Errorf("expected plan 1..3 (1 summary + 2 failures), got:\n%s", out)
	}
	if !strings.Contains(out, "not ok 1") {
		t.Errorf("expected summary not-ok at 1, got:\n%s", out)
	}
	if !strings.Contains(out, "not ok 2") || !strings.Contains(out, "not ok 3") {
		t.Errorf("expected per-failure not-ok lines, got:\n%s", out)
	}
	if !strings.Contains(out, "missing") {
		t.Errorf("missing reason in:\n%s", out)
	}
}

func TestTAP_Findings_FailWhenSeverityWarningOrAbove(t *testing.T) {
	res := output.NewResult("insider scan").WithBody(map[string]any{
		"findings": []any{
			map[string]any{"type": "novel_principal", "severity": "warning",
				"actor": "bob@acme", "reason": "new"},
			map[string]any{"type": "first_destructive", "severity": "critical",
				"actor": "alice@acme", "reason": "first kms.shred"},
		},
	})
	out := renderResult(t, res)
	if !strings.Contains(out, "1..3") {
		t.Errorf("expected plan 1..3 (1 summary + 2 findings), got:\n%s", out)
	}
	if !strings.Contains(out, "bob@acme") {
		t.Errorf("missing actor:\n%s", out)
	}
	if !strings.Contains(out, "first_destructive") {
		t.Errorf("missing finding type:\n%s", out)
	}
	for _, want := range []string{"not ok 2", "not ok 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// TestTAP_Findings_NoticeIsPass: a notice-severity finding (e.g.
// post_jit_destructive) does NOT fail.  Severity-warning-and-above
// is the failure threshold.  Wait — we treat warning as failure for
// the test-harness use case.  Notice is below warning so it passes.
func TestTAP_Findings_NoticeIsPass(t *testing.T) {
	res := output.NewResult("insider scan").WithBody(map[string]any{
		"findings": []any{
			map[string]any{"type": "post_jit_destructive", "severity": "notice",
				"actor": "alice", "reason": "break-glass"},
		},
	})
	out := renderResult(t, res)
	// Plan: 1 summary + 1 finding = 2 points
	if !strings.Contains(out, "1..2") {
		t.Errorf("plan: %s", out)
	}
	// Notice → ok (no not-ok lines).
	if strings.Contains(out, "not ok") {
		t.Errorf("notice should not fail, got:\n%s", out)
	}
}

func TestTAP_RenderEvent_FailureSeverity_NotOk(t *testing.T) {
	ev := &output.Event{
		Component:    "wal.stream",
		Op:           "lag_alert",
		Severity:     output.SeverityError,
		SeverityName: "error",
		Body:         map[string]any{"lag_seconds": 120},
	}
	var buf bytes.Buffer
	if err := tap.New().RenderEvent(&buf, ev); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "not ok 1 - [error] lag_alert\n") {
		t.Errorf("first line: %q", firstLine(out))
	}
	if !strings.Contains(out, "  ---") {
		t.Errorf("expected YAML diagnostic block on failure event")
	}
}

func TestTAP_RenderEvent_InfoSeverity_OK(t *testing.T) {
	ev := &output.Event{
		Component:    "backup",
		Op:           "started",
		Severity:     output.SeverityInfo,
		SeverityName: "info",
	}
	var buf bytes.Buffer
	if err := tap.New().RenderEvent(&buf, ev); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "ok 1 - [info] started\n") {
		t.Errorf("first line: %q", firstLine(out))
	}
	if strings.Contains(out, "---") {
		t.Errorf("expected no YAML diagnostic for ok event:\n%s", out)
	}
}

func TestTAP_NameAndSupportsTTY(t *testing.T) {
	r := tap.New()
	if r.Name() != "tap" {
		t.Errorf("Name = %q", r.Name())
	}
	if r.SupportsTTY() {
		t.Errorf("SupportsTTY should be false")
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestTAP_YAMLBlock_StringWithColonIsQuoted(t *testing.T) {
	res := output.NewResult("foo").WithError(
		output.NewError("foo.bar", "boom: with colon").
			WithSuggestion(&output.Suggestion{Human: "do this thing"}),
	)
	out := renderResult(t, res)
	// "boom: with colon" must be quoted to keep YAML parseable.
	if !strings.Contains(out, `"boom: with colon"`) {
		t.Errorf("colon in scalar not quoted in:\n%s", out)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
