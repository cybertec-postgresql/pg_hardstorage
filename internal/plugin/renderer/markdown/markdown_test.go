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
