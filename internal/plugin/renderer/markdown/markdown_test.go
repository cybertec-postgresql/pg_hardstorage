package markdown_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/markdown"
)

func TestMarkdown_RenderResult_HappyPath(t *testing.T) {
	r := markdown.New()
	res := output.NewResult("status").WithBody(map[string]any{
		"deployment": "db1",
		"healthy":    true,
	})
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{
		"# status",
		"*generated ",
		"## Result",
		"```json",
		`"deployment": "db1"`,
		"```\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestMarkdown_RenderResult_ErrorBlock(t *testing.T) {
	r := markdown.New()
	res := output.NewResult("x").WithError(output.NewError("wal.slot_missing",
		"Replication slot 'pg_hardstorage_db1' is not present").
		WithSuggestion(&output.Suggestion{
			Human:   "Recreate it",
			Command: "pg_hardstorage wal repair db1",
			DocURL:  "https://docs/runbooks/wal-slot-missing",
		}))
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{
		"## Status",
		"**ERROR**",
		"`wal.slot_missing`",
		"> Replication slot",
		"💡 Recreate it",
		"pg_hardstorage wal repair db1",
		"[docs](https://docs/runbooks/wal-slot-missing)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestMarkdown_RenderEvent_BulletShape(t *testing.T) {
	r := markdown.New()
	ev := output.NewEvent(output.SeverityWarning, "wal.stream", "lag_high").
		WithSubject(output.Subject{Deployment: "db1", Timeline: 3}).
		WithBody(map[string]any{"lag_seconds": 47})
	var buf bytes.Buffer
	if err := r.RenderEvent(&buf, ev); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.HasPrefix(got, "- **WARNING** `wal.stream/lag_high`") {
		t.Errorf("bullet should lead with severity + component/op; got:\n%s", got)
	}
	if !strings.Contains(got, "deployment=db1") {
		t.Errorf("subject missing; got:\n%s", got)
	}
	if !strings.Contains(got, "    ```json") {
		t.Errorf("nested code fence should be 4-space indented; got:\n%s", got)
	}
}

func TestMarkdown_RendererMetadata(t *testing.T) {
	r := markdown.New()
	if r.Name() != "markdown" {
		t.Errorf("Name = %q", r.Name())
	}
	if r.SupportsTTY() {
		t.Error("markdown is not TTY-friendly")
	}
}

// TestMarkdown_DocURLSchemeGated: a non-http(s) DocURL must render as
// inline code, not a clickable link — markdown viewed in a browser
// executes javascript:/data: link schemes (XSS). The HTML renderer
// gates this same field; markdown must match.
func TestMarkdown_DocURLSchemeGated(t *testing.T) {
	for _, tc := range []struct {
		url      string
		wantLink bool
	}{
		{"https://docs/ok", true},
		{"http://docs/ok", true},
		{"javascript:alert(1)", false},
		{"data:text/html,<script>alert(1)</script>", false},
		{"file:///etc/passwd", false},
	} {
		r := markdown.New()
		res := output.NewResult("x").WithError(output.NewError("c",
			"m").WithSuggestion(&output.Suggestion{DocURL: tc.url}))
		var buf bytes.Buffer
		if err := r.RenderResult(&buf, res); err != nil {
			t.Fatal(err)
		}
		got := buf.String()
		isLink := strings.Contains(got, "[docs]("+tc.url+")")
		if isLink != tc.wantLink {
			t.Errorf("DocURL %q: rendered as link=%v, want %v\n%s", tc.url, isLink, tc.wantLink, got)
		}
		if !tc.wantLink && !strings.Contains(got, "docs: `"+tc.url+"`") {
			t.Errorf("DocURL %q: unsafe scheme not rendered as inline code:\n%s", tc.url, got)
		}
	}
}
