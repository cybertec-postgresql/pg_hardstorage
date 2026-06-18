package csv_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/csv"
)

func TestCSV_RenderResult_HomogeneousList(t *testing.T) {
	r := csv.New()
	res := output.NewResult("list").WithBody([]map[string]any{
		{"name": "alice", "age": 30},
		{"name": "bob", "age": 31},
	})
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	// Header is the lex-sorted union of keys: age,name
	if !strings.HasPrefix(got, "age,name\n") {
		t.Errorf("header should be age,name (sorted union); got: %s", got)
	}
	for _, want := range []string{"30,alice", "31,bob"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing row %q; got:\n%s", want, got)
		}
	}
}

func TestCSV_RenderResult_MapBody_ProducesKVRows(t *testing.T) {
	r := csv.New()
	res := output.NewResult("status").WithBody(map[string]any{
		"deployment": "db1",
		"healthy":    true,
	})
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{"key,value", "deployment,db1", "healthy,true"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestCSV_Sanitize_DefendsAgainstFormulaInjection(t *testing.T) {
	r := csv.New()
	// Build a body whose values are formula-shaped.
	res := output.NewResult("evil").WithBody(map[string]any{
		"a": "=SUM(A1:A10)",
		"b": "+1+1",
		"c": "@cmd",
		"d": "-1",
	})
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	// Each formula-shaped value should be quote-prefixed.
	for _, prefix := range []string{`a,'=SUM(A1:A10)`, `b,'+1+1`, `c,'@cmd`, `d,'-1`} {
		if !strings.Contains(got, prefix) {
			t.Errorf("missing sanitised %q in:\n%s", prefix, got)
		}
	}
}

func TestCSV_RenderResult_ErrorBecomesKVForm(t *testing.T) {
	r := csv.New()
	res := output.NewResult("x").WithError(output.NewError("foo.bar", "boom").
		WithSuggestion(&output.Suggestion{Human: "fix it"}))
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{"key,value", "code,foo.bar", "message,boom", "suggestion_human,fix it"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestCSV_RenderEvent_OneRowPerEvent(t *testing.T) {
	r := csv.New()
	ev := output.NewEvent(output.SeverityWarning, "wal.stream", "lag_high").
		WithSubject(output.Subject{Deployment: "db1", Timeline: 3}).
		WithBody(map[string]any{"lag_seconds": 47})
	var buf bytes.Buffer
	if err := r.RenderEvent(&buf, ev); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{
		"warning", "wal.stream", "lag_high", "db1", "lag_seconds",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in event row:\n%s", want, got)
		}
	}
}

func TestCSV_RendererMetadata(t *testing.T) {
	r := csv.New()
	if r.Name() != "csv" {
		t.Errorf("Name = %q", r.Name())
	}
	if r.SupportsTTY() {
		t.Error("CSV is not TTY-friendly")
	}
}
