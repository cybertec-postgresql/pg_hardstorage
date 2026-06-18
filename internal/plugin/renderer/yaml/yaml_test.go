package yaml_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/yaml"
)

func TestYAML_RendersResult(t *testing.T) {
	r := yaml.New()
	res := output.NewResult("doctor").WithBody(map[string]any{
		"healthy": true,
		"issues":  []string{"a", "b"},
	})
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatalf("RenderResult: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		"---",
		"schema: pg_hardstorage.v1",
		"command: doctor",
		"healthy: true",
		"- a",
		"- b",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("yaml missing %q in:\n%s", want, got)
		}
	}
}

func TestYAML_RendersEvent(t *testing.T) {
	r := yaml.New()
	ev := output.NewEvent(output.SeverityWarning, "wal", "lag")
	ev.Subject = output.Subject{Deployment: "db1"}
	var buf bytes.Buffer
	if err := r.RenderEvent(&buf, ev); err != nil {
		t.Fatalf("RenderEvent: %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		"severity_name: warning",
		"component: wal",
		"op: lag",
		"deployment: db1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("yaml missing %q in:\n%s", want, got)
		}
	}
}

func TestYAML_SupportsTTYIsFalse(t *testing.T) {
	if yaml.New().SupportsTTY() {
		t.Error("YAML renderer should not declare TTY support")
	}
}
