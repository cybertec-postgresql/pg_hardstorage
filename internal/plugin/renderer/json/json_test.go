package json_test

import (
	"bytes"
	stdjson "encoding/json"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/json"
)

func TestRenderer_NameAndTTY(t *testing.T) {
	r := json.New()
	if r.Name() != "json" {
		t.Errorf("Name() = %q", r.Name())
	}
	if r.SupportsTTY() {
		t.Error("JSON should not declare TTY support")
	}
}

func TestRenderResult_Success(t *testing.T) {
	r := json.New()
	res := output.NewResult("status").WithBody(map[string]any{"deployments": []string{"db1"}})
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatalf("render: %v", err)
	}
	// Validate it parses back to a Result.
	var got output.Result
	if err := stdjson.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unparseable JSON: %v\n%s", err, buf.String())
	}
	if got.Schema != output.Schema {
		t.Errorf("schema lost in round-trip: %q", got.Schema)
	}
	if got.Command != "status" {
		t.Errorf("command lost: %q", got.Command)
	}
	// Indented (two-space).
	if !strings.Contains(buf.String(), "\n  \"") {
		t.Errorf("expected indented output; got %s", buf.String())
	}
}

func TestRenderResult_Error(t *testing.T) {
	r := json.New()
	res := output.NewResult("backup").WithError(
		output.NewError("wal.slot_missing", "slot dropped").
			WithSuggestion(&output.Suggestion{Command: "pg_hardstorage wal repair db1"}))
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatalf("render: %v", err)
	}
	s := buf.String()
	for _, want := range []string{`"code": "wal.slot_missing"`, `"command": "pg_hardstorage wal repair db1"`} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q\n%s", want, s)
		}
	}
}

func TestRenderEvent(t *testing.T) {
	r := json.New()
	ev := output.NewEvent(output.SeverityInfo, "backup", "started").
		WithSubject(output.Subject{Deployment: "db1"})
	var buf bytes.Buffer
	if err := r.RenderEvent(&buf, ev); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), `"deployment": "db1"`) {
		t.Errorf("output missing deployment\n%s", buf.String())
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Error("JSON encoder should add a trailing newline")
	}
}

func TestEscapeHTMLDisabled(t *testing.T) {
	r := json.New()
	res := output.NewResult("show").WithBody(map[string]string{"q": "a < b && c > d"})
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	// HTML escaping would turn < into \u003c; we want raw output for tooling.
	if strings.Contains(buf.String(), `\u003c`) {
		t.Errorf("HTML escape should be disabled\n%s", buf.String())
	}
}
