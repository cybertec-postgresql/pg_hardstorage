package text_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/text"
)

func TestNameAndTTY(t *testing.T) {
	r := text.New()
	if r.Name() != "text" {
		t.Errorf("Name() = %q", r.Name())
	}
	if !r.SupportsTTY() {
		t.Error("text should declare TTY support")
	}
}

func TestRenderResult_StringBody(t *testing.T) {
	r := text.New()
	res := output.NewResult("version").WithBody("pg_hardstorage dev (none, built X)")
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatalf("render: %v", err)
	}
	got := buf.String()
	want := "pg_hardstorage dev (none, built X)\n"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRenderResult_NilBody(t *testing.T) {
	r := text.New()
	res := output.NewResult("noop")
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatalf("render: %v", err)
	}
	if buf.String() != "" {
		t.Errorf("nil body should produce no output; got %q", buf.String())
	}
}

type stringerBody struct{ Text string }

func (s stringerBody) WriteText(w io.Writer) error {
	_, err := w.Write([]byte(s.Text))
	return err
}

func TestRenderResult_TextWriterBody(t *testing.T) {
	r := text.New()
	res := output.NewResult("status").WithBody(stringerBody{Text: "all clear"})
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "all clear\n" {
		t.Errorf("got %q", buf.String())
	}
}

func TestRenderResult_FallbackJSON(t *testing.T) {
	r := text.New()
	res := output.NewResult("show").WithBody(map[string]string{"k": "v"})
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"k": "v"`) {
		t.Errorf("expected pretty JSON fallback; got %q", buf.String())
	}
}

func TestRenderResult_Error(t *testing.T) {
	r := text.New()
	res := output.NewResult("wal stream").WithError(
		output.NewError("wal.slot_missing", "slot dropped").
			WithSubject(output.Subject{Deployment: "db1"}).
			WithSuggestion(&output.Suggestion{
				Human:   "the slot was probably dropped",
				Command: "pg_hardstorage wal repair db1",
				DocURL:  "https://docs.pghardstorage.org/runbooks/wal-slot-missing",
			}))
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	for _, want := range []string{
		"ERROR wal.slot_missing: slot dropped",
		"subject: deployment=db1",
		"hint: the slot was probably dropped",
		"run: pg_hardstorage wal repair db1",
		"docs: https://docs.pghardstorage.org/runbooks/wal-slot-missing",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q\n%s", want, s)
		}
	}
}

func TestRenderEvent_StringBody_HonoursPrefix(t *testing.T) {
	r := text.New()
	ev := output.NewEvent(output.SeverityInfo, "backup", "started").WithBody("hello")
	var buf bytes.Buffer
	if err := r.RenderEvent(&buf, ev); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "  body: hello") {
		t.Errorf("string body should render with the body: prefix; got\n%s", buf.String())
	}
}

func TestRenderEvent_JSONBody_HonoursPrefixOnFirstLine(t *testing.T) {
	r := text.New()
	ev := output.NewEvent(output.SeverityInfo, "backup", "stats").
		WithBody(map[string]any{"files": 42, "bytes": 1024})
	var buf bytes.Buffer
	if err := r.RenderEvent(&buf, ev); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// The first body line MUST start with the "body: " prefix the
	// caller passed in, not a bare "  {" — this was the
	// renderBodyIndented bug a reviewer flagged.
	if !strings.Contains(out, "  body: {") {
		t.Errorf("JSON body's first line should carry the prefix; got\n%s", out)
	}
	// Continuation lines align under the prefix content (len("  body: ") = 8).
	if !strings.Contains(out, "        \"") {
		t.Errorf("JSON continuation lines should be indented 8 spaces under the prefix; got\n%s", out)
	}
}

func TestRenderEvent_Header(t *testing.T) {
	r := text.New()
	ev := output.NewEvent(output.SeverityWarning, "wal.stream", "lag_high").
		WithSubject(output.Subject{Deployment: "db1", Timeline: 3})
	var buf bytes.Buffer
	if err := r.RenderEvent(&buf, ev); err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	for _, want := range []string{
		"[WARNING]",
		"wal.stream.lag_high",
		"deployment=db1",
		"timeline=3",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}
