// Package csv is an output.Renderer that emits CSV tables.
//
// Useful for fleet exports — pipe `pg_hardstorage list db1 -o csv |
// csvkit/excel/ledger`. Best on commands whose Result body is a
// list of homogeneous records (the `list`, `notify list`,
// `deployment list`, `wal list` commands all qualify); for other
// shapes we fall back to a 2-column key/value form.
//
// The renderer is paranoid about CSV-injection: any cell beginning
// with `=`, `+`, `-`, `@`, tab, or carriage return is prefixed with
// a single quote so spreadsheet tools don't interpret it as a
// formula. (`/`-prefix, `\t`, and `\r` are the most-cited
// CSV-injection vectors per OWASP.)
package csv

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/jsonshape"
)

// Renderer emits CSV. Stateless — every method is safe to call
// concurrently as long as the underlying Writer is.
type Renderer struct{}

// New returns a Renderer.
func New() *Renderer { return &Renderer{} }

// Name implements output.Renderer.
func (r *Renderer) Name() string { return "csv" }

// SupportsTTY implements output.Renderer. CSV isn't a TTY-friendly
// shape (a humans-friendly form would be `text`).
func (r *Renderer) SupportsTTY() bool { return false }

// Close implements output.Renderer.
func (r *Renderer) Close() error { return nil }

// RenderResult implements output.Renderer.
//
// Strategy:
//
//   - Error result → 2-column "field,value" with the error fields.
//   - Body is a slice of homogeneous structs/maps → header row +
//     one row per element. The lex-sorted union of keys is used
//     for header stability.
//   - Anything else (scalar, single struct/map) → flatten via JSON
//     marshal/unmarshal into a map and emit a 2-column form.
//
// We lean on the body's JSON marshaling rather than reflecting on
// arbitrary Go types — that gives us stable lowercase column names
// (matching the JSON renderer's output) and handles every shape the
// CLI emits today without per-command knowledge here.
func (r *Renderer) RenderResult(w io.Writer, res *output.Result) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	if res.IsError() {
		return writeKVRows(cw, errorAsMap(res.Error))
	}
	if res.Result == nil {
		return nil
	}

	// Round-trip through JSON to normalise to (map[string]any |
	// []any | scalar). This is the same shape the json renderer
	// emits, so the on-disk JSON and CSV are perfectly aligned.
	v, err := jsonshape.RoundTrip(res.Result)
	if err != nil {
		return fmt.Errorf("csv: marshal body: %w", err)
	}
	return writeAny(cw, v)
}

// RenderEvent implements output.Renderer. One row per event. The
// header is implicit; each event includes its own column set so a
// stream of mixed-shape events still produces one row each. CSV
// streams aren't really designed for shape-evolving data, but this
// is the least-surprising rendering we can do.
func (r *Renderer) RenderEvent(w io.Writer, ev *output.Event) error {
	cw := csv.NewWriter(w)
	row := []string{
		ev.GeneratedAt.UTC().Format("2006-01-02T15:04:05.000000Z"),
		ev.SeverityName,
		ev.Component,
		ev.Op,
		ev.Subject.Deployment,
		ev.Subject.BackupID,
		fmt.Sprintf("%d", ev.Subject.Timeline),
		ev.Subject.LSN,
		flatBody(ev.Body),
	}
	for i, c := range row {
		row[i] = sanitize(c)
	}
	if err := cw.Write(row); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

// writeAny dispatches on the JSON-shaped value.
func writeAny(cw *csv.Writer, v any) error {
	switch x := v.(type) {
	case []any:
		return writeSlice(cw, x)
	case map[string]any:
		// Every list-shaped command wraps its rows in an envelope map
		// ({deployment, count, backups: [...]}), so without unwrapping
		// the tabular path was UNREACHABLE from any real command — the
		// exact use case the package doc names ("pipe list -o csv into
		// csvkit/excel") produced a 2-column key/value dump with the
		// row array crammed into one escaped-JSON cell. When the map
		// is exactly "scalars + one array of objects", the array IS
		// the table: render it. Anything more ambiguous still falls
		// back to key/value.
		if rows, ok := envelopeRows(x); ok {
			return writeSlice(cw, rows)
		}
		return writeKVRows(cw, x)
	default:
		// Scalar — single cell.
		return cw.Write([]string{sanitize(fmt.Sprintf("%v", x))})
	}
}

// envelopeRows detects the common list-command envelope: a map whose
// values are scalars/null plus EXACTLY ONE non-empty []any in which
// every element is an object. Returns that array. Multiple arrays,
// arrays of scalars, or nested-map fields disqualify the shape (the
// caller then uses the key/value fallback).
func envelopeRows(m map[string]any) ([]any, bool) {
	var rows []any
	found := false
	for _, v := range m {
		switch x := v.(type) {
		case []any:
			if len(x) == 0 {
				continue // empty list — nothing tabular to prefer
			}
			for _, el := range x {
				if _, isObj := el.(map[string]any); !isObj {
					return nil, false // array of scalars — not rows
				}
			}
			if found {
				return nil, false // two candidate tables — ambiguous
			}
			rows, found = x, true
		case map[string]any:
			return nil, false // nested object — not a plain envelope
		default:
			// scalar / null — envelope metadata (count, deployment, …)
		}
	}
	return rows, found
}

// writeSlice emits a header row + one row per element. Heterogeneous
// element types fall back to per-element key/value rows separated by
// blank rows.
func writeSlice(cw *csv.Writer, items []any) error {
	if len(items) == 0 {
		return nil
	}
	// Are all elements maps? Then we have a homogeneous-table shape.
	allMap := true
	for _, it := range items {
		if _, ok := it.(map[string]any); !ok {
			allMap = false
			break
		}
	}
	if !allMap {
		// Heterogeneous — emit each element as its own block.
		for i, it := range items {
			if i > 0 {
				_ = cw.Write([]string{}) // blank row separator
			}
			if err := writeAny(cw, it); err != nil {
				return err
			}
		}
		return nil
	}
	// Union of keys across all rows, sorted for header stability.
	keySet := map[string]struct{}{}
	for _, it := range items {
		for k := range it.(map[string]any) {
			keySet[k] = struct{}{}
		}
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if err := cw.Write(keys); err != nil {
		return err
	}
	for _, it := range items {
		m := it.(map[string]any)
		row := make([]string, len(keys))
		for i, k := range keys {
			row[i] = sanitize(stringify(m[k]))
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	return nil
}

// writeKVRows emits a 2-column form for map-shaped bodies. Keys
// sorted for determinism.
func writeKVRows(cw *csv.Writer, m map[string]any) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if err := cw.Write([]string{"key", "value"}); err != nil {
		return err
	}
	for _, k := range keys {
		if err := cw.Write([]string{sanitize(k), sanitize(stringify(m[k]))}); err != nil {
			return err
		}
	}
	return nil
}

// stringify renders a JSON-shaped value for a CSV cell. Scalars come
// out via fmt.Sprintf; nested maps/slices are JSON-marshalled so
// the cell is one logical chunk that pandas / csvkit can pivot on.
func stringify(v any) string {
	if v == nil {
		return ""
	}
	switch v.(type) {
	case map[string]any, []any:
		bs, _ := json.Marshal(v)
		return string(bs)
	}
	return fmt.Sprintf("%v", v)
}

// flatBody is the per-event body cell. JSON-encode if non-scalar
// so the cell remains a single semicolon-free chunk.
func flatBody(body any) string {
	if body == nil {
		return ""
	}
	if reflect.ValueOf(body).Kind() == reflect.String {
		return body.(string)
	}
	bs, err := json.Marshal(body)
	if err != nil {
		return fmt.Sprintf("%v", body)
	}
	return string(bs)
}

// sanitize defends against CSV injection attacks: cells that begin
// with =, +, -, @, tab, or CR are interpreted as formulas by Excel,
// LibreOffice, and Google Sheets. Prefix with a single quote — the
// universally-honoured "this is data, not a formula" sigil.
//
// References:
//   - OWASP: "CSV Injection"
//   - https://cwe.mitre.org/data/definitions/1236.html
func sanitize(cell string) string {
	if cell == "" {
		return cell
	}
	switch cell[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + cell
	}
	return cell
}

// errorAsMap projects a structured Error into a map for the
// 2-column error rendering path.
func errorAsMap(e *output.Error) map[string]any {
	m := map[string]any{
		"code":    e.Code,
		"message": e.Message,
	}
	if e.Suggestion != nil {
		if e.Suggestion.Human != "" {
			m["suggestion_human"] = e.Suggestion.Human
		}
		if e.Suggestion.Command != "" {
			m["suggestion_command"] = e.Suggestion.Command
		}
		if e.Suggestion.DocURL != "" {
			m["suggestion_doc_url"] = e.Suggestion.DocURL
		}
	}
	return m
}
