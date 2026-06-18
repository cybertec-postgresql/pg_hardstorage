package ndjson_test

import (
	"bytes"
	stdjson "encoding/json"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/ndjson"
)

func TestNameAndTTY(t *testing.T) {
	r := ndjson.New()
	if r.Name() != "ndjson" {
		t.Errorf("Name() = %q", r.Name())
	}
	if r.SupportsTTY() {
		t.Error("NDJSON should not declare TTY support")
	}
}

func TestRenderEvent_OneLineOneNewline(t *testing.T) {
	r := ndjson.New()
	var buf bytes.Buffer
	for i := 0; i < 3; i++ {
		ev := output.NewEvent(output.SeverityInfo, "backup", "progress").
			WithBody(map[string]int{"step": i})
		if err := r.RenderEvent(&buf, ev); err != nil {
			t.Fatalf("render: %v", err)
		}
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), buf.String())
	}
	for i, line := range lines {
		if strings.Contains(line, "\n") {
			t.Errorf("line %d contains an embedded newline: %q", i, line)
		}
		var ev output.Event
		if err := stdjson.Unmarshal([]byte(line), &ev); err != nil {
			t.Errorf("line %d unparseable: %v\n%s", i, err, line)
		}
	}
}

func TestRenderResult_SingleLine(t *testing.T) {
	r := ndjson.New()
	res := output.NewResult("status").WithBody(map[string]string{"a": "b"})
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Count(out, "\n") != 1 {
		t.Errorf("NDJSON Result should be one line; got %q", out)
	}
}
