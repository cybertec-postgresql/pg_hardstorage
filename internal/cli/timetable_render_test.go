package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRenderArgsJSON_EscapesControlCharacters: the property the
// hand-rolled jsonQuote got wrong — control characters in the
// argv (e.g. an operator passing a tab or newline through
// --deployment) must produce valid JSON. encoding/json.Marshal
// handles every control char correctly; we assert by round-trip
// (parse the output and confirm we get the same []string back).
func TestRenderArgsJSON_EscapesControlCharacters(t *testing.T) {
	args := []string{"wal", "audit", "{{deployment}}", "--repo", "{{repo}}"}
	f := timetableEmitFlags{
		repoURL:    "s3://bucket/path",
		deployment: "weird\nwith\ttabs\rand\bbackspace",
	}

	out := renderArgsJSON(args, f)

	// Must parse as valid JSON — the manual jsonQuote would
	// have produced a literal newline mid-string, breaking JSON.
	var got []string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("renderArgsJSON output is not valid JSON: %v\nout=%q", err, out)
	}
	want := []string{
		"wal", "audit",
		"weird\nwith\ttabs\rand\bbackspace",
		"--repo", "s3://bucket/path",
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d; got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// And the result is a single line (one of the original
	// rationales for the hand-rolled path).
	if strings.Contains(out, "\n") {
		t.Errorf("renderArgsJSON should be one line; got:\n%s", out)
	}
}

// TestRenderArgsJSON_EscapesEmbeddedQuotes: a deployment whose
// substituted value contains a double quote (e.g., a hostile or
// just-mistyped flag) round-trips correctly. The old jsonQuote
// did handle double quotes — we keep the regression guard.
func TestRenderArgsJSON_EscapesEmbeddedQuotes(t *testing.T) {
	args := []string{"x", "{{deployment}}"}
	f := timetableEmitFlags{deployment: `prod"with"quotes`}

	out := renderArgsJSON(args, f)
	var got []string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not valid JSON: %v\nout=%q", err, out)
	}
	if got[1] != `prod"with"quotes` {
		t.Errorf("[1] = %q, want %q", got[1], `prod"with"quotes`)
	}
}

// TestRenderArgsJSON_EscapesBackslashes: backslashes in
// substitutions (Windows-ish paths or escaped strings) must
// round-trip. Old jsonQuote handled this; we keep the guard.
func TestRenderArgsJSON_EscapesBackslashes(t *testing.T) {
	args := []string{"--repo", "{{repo}}"}
	f := timetableEmitFlags{repoURL: `C:\Users\backup\repo`}

	out := renderArgsJSON(args, f)
	var got []string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not valid JSON: %v\nout=%q", err, out)
	}
	if got[1] != `C:\Users\backup\repo` {
		t.Errorf("[1] = %q, want %q", got[1], `C:\Users\backup\repo`)
	}
}
