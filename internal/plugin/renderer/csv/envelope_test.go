package csv

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Regression: every list command wraps its rows in an envelope map, so
// the tabular path was unreachable — `list -o csv` produced a 2-column
// key/value dump with the row array as one escaped-JSON cell. The
// envelope's single array-of-objects must render as header + rows.
func TestRenderResult_ListEnvelopeBecomesTable(t *testing.T) {
	body := map[string]any{
		"deployment": "db1",
		"count":      2,
		"backups": []any{
			map[string]any{"backup_id": "b1", "files": 10},
			map[string]any{"backup_id": "b2", "files": 12},
		},
	}
	var buf bytes.Buffer
	r := New()
	res := output.NewResult("pg_hardstorage list").WithBody(body)
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatalf("render: %v", err)
	}
	got := buf.String()
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 rows, got %d lines:\n%s", len(lines), got)
	}
	if !strings.Contains(lines[0], "backup_id") || !strings.Contains(lines[0], "files") {
		t.Errorf("header row missing columns: %q", lines[0])
	}
	if strings.Contains(got, "{") {
		t.Errorf("CSV still contains an escaped-JSON blob:\n%s", got)
	}
}

// Ambiguous / non-envelope shapes must keep the key/value fallback.
func TestEnvelopeRows_AmbiguousShapesFallBack(t *testing.T) {
	cases := []map[string]any{
		{"a": []any{map[string]any{"x": 1}}, "b": []any{map[string]any{"y": 2}}}, // two tables
		{"a": []any{"s1", "s2"}},           // scalar array
		{"a": map[string]any{"nested": 1}}, // nested object
		{"a": 1, "b": "s"},                 // no array at all
	}
	for i, m := range cases {
		if _, ok := envelopeRows(m); ok {
			t.Errorf("case %d: envelopeRows unexpectedly matched %v", i, m)
		}
	}
}
