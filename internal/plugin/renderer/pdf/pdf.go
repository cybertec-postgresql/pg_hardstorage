// Package pdf is an output.Renderer that emits PDF documents
// scoped to the project's compliance / DSA / integrity report use
// cases.  Closes the SPEC commitment for the `pdf-report`
// renderer:
//
//	"Renderers: pdf-report (compliance) — auto-generated
//	monthly PDF: 'all backups encrypted with FIPS-validated module,
//	all verifications passing, RPO met for X days.'  Maps to SOC 2
//	/ ISO 27001 / HIPAA / PCI / FedRAMP control IDs."
//
// We're conservative about vendor surface: the implementation uses
// our in-house pdfwriter (a small pure-Go PDF generator scoped to
// what reports need — text, headings, simple tables, footer) rather
// than vendoring a third-party PDF library.  The cost is that
// rich-PDF features (embedded fonts, images, vector graphics) are
// out of scope; the benefit is no new vendor surface and a hand-
// reviewable codebase.
//
// Strategy:
//
//   - Result with no error: title + metadata block + body.  The
//     body's shape derives from common report keys (`schema`,
//     `id`, `tenant`, `status`, `met`, `affected_backups`, etc.);
//     unknown shapes fall back to a flat key-value paragraph.
//   - Result with error: title + structured error block.
//   - Streaming Event: one PDF per event.  Useful as a regression
//     guard rather than a real workflow — operators producing
//     PDF reports almost always go through Result-shaped commands
//     (`compliance report`, `dsa show`, `integrity show`).
package pdf

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/jsonshape"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/pdf/pdfwriter"
)

// Renderer emits PDF.  Stateless across calls.
type Renderer struct{}

// New returns a Renderer.
func New() *Renderer { return &Renderer{} }

// Name implements output.Renderer.
func (r *Renderer) Name() string { return "pdf" }

// SupportsTTY implements output.Renderer.  PDF is binary; never
// suitable for a TTY.
func (r *Renderer) SupportsTTY() bool { return false }

// Close implements output.Renderer.
func (r *Renderer) Close() error { return nil }

// RenderResult writes a one-shot PDF document to w.  Layout:
//
//  1. Centred title (the command name)
//  2. Generated-at timestamp
//  3. Per-result body:
//     - error-shaped: a heading + a code/message/suggestion table
//     - body-shaped: every top-level key in the body becomes a
//     heading + tabular detail (lists become tables; maps
//     become key-value paragraphs; scalars become paragraphs)
//  4. Footer: `pg_hardstorage <command> · <generated_at>` on every
//     page.
func (r *Renderer) RenderResult(w io.Writer, res *output.Result) error {
	d := pdfwriter.New(pdfwriter.Options{
		Compress: true,
	})
	command := res.Command
	if command == "" {
		command = "pg_hardstorage report"
	}
	d.SetFooter(fmt.Sprintf("%s · %s",
		command, res.GeneratedAt.Format(time.RFC3339)))
	d.AddTitle(command)
	d.AddParagraph(fmt.Sprintf("Generated at %s",
		res.GeneratedAt.Format(time.RFC3339)))

	if res.IsError() {
		renderError(d, res.Error)
	} else {
		renderBody(d, res.Result)
	}
	_, err := d.WriteTo(w)
	return err
}

// RenderEvent writes a single-event PDF.  As noted in the package
// comment, this exists as a regression guard — the workflows that
// produce PDFs are Result-shaped, not Event-streamed.
func (r *Renderer) RenderEvent(w io.Writer, ev *output.Event) error {
	d := pdfwriter.New(pdfwriter.Options{Compress: true})
	d.SetFooter(fmt.Sprintf("event · %s", ev.GeneratedAt.Format(time.RFC3339)))
	d.AddTitle(fmt.Sprintf("[%s] %s · %s",
		ev.SeverityName, ev.Component, ev.Op))
	d.AddParagraph(fmt.Sprintf("Generated at %s",
		ev.GeneratedAt.Format(time.RFC3339)))
	if ev.Body != nil {
		d.AddHeading2("Body")
		renderBody(d, ev.Body)
	}
	if ev.Suggestion != nil {
		renderSuggestion(d, ev.Suggestion)
	}
	_, err := d.WriteTo(w)
	return err
}

// renderError emits the structured-error block.
func renderError(d *pdfwriter.Doc, e *output.Error) {
	d.AddHeading1("Error")
	rows := [][]string{
		{"code", e.Code},
		{"message", e.Message},
	}
	d.AddTable([]string{"field", "value"}, rows)
	if e.Suggestion != nil {
		renderSuggestion(d, e.Suggestion)
	}
}

func renderSuggestion(d *pdfwriter.Doc, s *output.Suggestion) {
	d.AddHeading2("Suggestion")
	rows := [][]string{}
	if s.Human != "" {
		rows = append(rows, []string{"human", s.Human})
	}
	if s.Command != "" {
		rows = append(rows, []string{"command", s.Command})
	}
	if s.DocURL != "" {
		rows = append(rows, []string{"doc_url", s.DocURL})
	}
	if len(rows) > 0 {
		d.AddTable([]string{"field", "value"}, rows)
	}
}

// renderBody walks a result body and emits sections.  We round-trip
// through JSON so the renderer doesn't depend on per-command type
// shapes.
func renderBody(d *pdfwriter.Doc, body any) {
	if body == nil {
		return
	}
	v, err := jsonshape.RoundTrip(body)
	if err != nil {
		d.AddParagraph(fmt.Sprintf("(unable to marshal body: %v)", err))
		return
	}
	switch x := v.(type) {
	case map[string]any:
		renderMap(d, x)
	case []any:
		renderList(d, "items", x)
	default:
		d.AddParagraph(fmt.Sprintf("%v", x))
	}
}

// renderMap renders a top-level map.  Scalar entries become a key-
// value table; nested-map entries become headings; nested-list
// entries become tables.
func renderMap(d *pdfwriter.Doc, m map[string]any) {
	scalarRows := [][]string{}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Two passes: scalars + flat-string maps first as a single
	// summary table; then complex sub-sections (nested maps /
	// lists of maps) as their own headings.
	for _, k := range keys {
		v := m[k]
		switch v.(type) {
		case map[string]any, []any:
			continue
		}
		scalarRows = append(scalarRows, []string{k, scalarString(v)})
	}
	if len(scalarRows) > 0 {
		d.AddHeading1("Summary")
		d.AddTable([]string{"field", "value"}, scalarRows)
	}
	for _, k := range keys {
		v := m[k]
		switch x := v.(type) {
		case map[string]any:
			d.AddHeading1(humanize(k))
			renderMap(d, x)
		case []any:
			renderList(d, k, x)
		}
	}
}

// renderList emits one heading + a table per list-of-maps; for
// lists of scalars we emit a single-column table.
func renderList(d *pdfwriter.Doc, key string, items []any) {
	if len(items) == 0 {
		return
	}
	d.AddHeading1(humanize(key))
	// Are all items maps?
	allMap := true
	for _, item := range items {
		if _, ok := item.(map[string]any); !ok {
			allMap = false
			break
		}
	}
	if allMap {
		// Union of keys across all rows, lex-sorted.
		keySet := map[string]struct{}{}
		for _, item := range items {
			for k := range item.(map[string]any) {
				keySet[k] = struct{}{}
			}
		}
		hdrs := make([]string, 0, len(keySet))
		for k := range keySet {
			hdrs = append(hdrs, k)
		}
		sort.Strings(hdrs)
		rows := make([][]string, 0, len(items))
		for _, item := range items {
			m := item.(map[string]any)
			row := make([]string, len(hdrs))
			for i, h := range hdrs {
				row[i] = scalarString(m[h])
			}
			rows = append(rows, row)
		}
		d.AddTable(hdrs, rows)
		return
	}
	// List of scalars / mixed.  Single-column table.
	rows := make([][]string, 0, len(items))
	for _, item := range items {
		rows = append(rows, []string{scalarString(item)})
	}
	d.AddTable([]string{"value"}, rows)
}

// humanize turns "manifests_affected" into "Manifests Affected"
// for a friendlier section heading.
func humanize(key string) string {
	parts := strings.FieldsFunc(key, func(r rune) bool {
		return r == '_' || r == '-' || r == '.'
	})
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// scalarString renders a JSON-shape value as a single line.  Maps
// and lists fall through to compact JSON so the cell at least
// shows the shape; reports that need richer rendering of nested
// structures should use the JSON renderer.
func scalarString(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case bool, int, int64, float64:
		return fmt.Sprintf("%v", x)
	case map[string]any, []any:
		bs, _ := json.Marshal(x)
		s := string(bs)
		if len(s) > 200 {
			return s[:197] + "…"
		}
		return s
	}
	return fmt.Sprintf("%v", v)
}
