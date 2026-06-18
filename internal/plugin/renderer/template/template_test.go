package template_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	tplrenderer "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/template"
)

func TestTemplate_RendersField(t *testing.T) {
	r, err := tplrenderer.New("{{.command}}: {{.result.healthy}}")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res := output.NewResult("doctor").WithBody(map[string]any{"healthy": true})
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatalf("RenderResult: %v", err)
	}
	if got := buf.String(); got != "doctor: true" {
		t.Errorf("got %q, want %q", got, "doctor: true")
	}
}

func TestTemplate_RendersList(t *testing.T) {
	r, err := tplrenderer.New(`{{range .result.backups}}{{.id}}{{"\n"}}{{end}}`)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res := output.NewResult("list").WithBody(map[string]any{
		"backups": []map[string]any{
			{"id": "db1.full.1"},
			{"id": "db1.full.2"},
		},
	})
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatalf("RenderResult: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"db1.full.1", "db1.full.2"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q: %q", want, got)
		}
	}
}

func TestTemplate_RendersEvent(t *testing.T) {
	r, err := tplrenderer.New("{{.severity_name}} {{.component}}.{{.op}} {{.subject.deployment}}")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ev := output.NewEvent(output.SeverityWarning, "wal", "lag")
	ev.Subject = output.Subject{Deployment: "db1"}
	var buf bytes.Buffer
	if err := r.RenderEvent(&buf, ev); err != nil {
		t.Fatalf("RenderEvent: %v", err)
	}
	if got := buf.String(); got != "warning wal.lag db1" {
		t.Errorf("got %q", got)
	}
}

func TestTemplate_RejectsEmpty(t *testing.T) {
	if _, err := tplrenderer.New(""); err == nil {
		t.Error("expected error for empty template")
	}
}

func TestTemplate_RejectsBadSyntax(t *testing.T) {
	if _, err := tplrenderer.New("{{.unterminated"); err == nil {
		t.Error("expected parse error for malformed template")
	}
}

func TestTemplate_RuntimeErrorFromIllegalCall(t *testing.T) {
	// Calling a non-existent function surfaces a parse error,
	// not a runtime error.  An actual runtime error: index out
	// of range on a list.
	r, _ := tplrenderer.New("{{(index .result.backups 99).id}}")
	res := output.NewResult("list").WithBody(map[string]any{
		"backups": []map[string]any{{"id": "only-one"}},
	})
	var buf bytes.Buffer
	err := r.RenderResult(&buf, res)
	if err == nil {
		t.Error("expected runtime error from out-of-range index")
	}
}
