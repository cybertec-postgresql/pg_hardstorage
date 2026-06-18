// Package text is the default human-readable renderer for the CLI.
//
// It prefers terse, actionable output. Types that implement TextWriter
// render themselves; everything else falls back to indented JSON. Errors
// get a structured layout with the Suggestion printed as next-steps so
// the 3am operator sees the remediation immediately.
//
// This package will grow ANSI colour, ASCII tables, and progress bars as
// the implementation matures. For now it stays simple and obvious.
package text

import (
	stdjson "encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Name is the canonical name of this renderer.
const Name = "text"

// TextWriter is an opt-in interface for nicely-formatted text output.
// Types that implement it (e.g. version info, status snapshots) take full
// control of their text representation. Implementations should NOT include
// a trailing newline — the renderer adds one.
type TextWriter interface {
	WriteText(w io.Writer) error
}

// Renderer is the human-readable renderer.
type Renderer struct {
	// NoColor suppresses ANSI codes. Honored when (and if) colour is added.
	NoColor bool
}

// New returns a text renderer.
func New() *Renderer { return &Renderer{} }

// Name implements output.Renderer.
func (r *Renderer) Name() string { return Name }

// SupportsTTY reports true — text is the default for interactive use.
func (r *Renderer) SupportsTTY() bool { return true }

// Close is a no-op for now.
func (r *Renderer) Close() error { return nil }

// RenderResult writes the Result. Errors take a structured layout;
// successes delegate to TextWriter or fall back to JSON.
func (r *Renderer) RenderResult(w io.Writer, res *output.Result) error {
	if res == nil {
		return nil
	}
	if res.IsError() {
		return r.renderError(w, res.Error)
	}
	return r.renderBody(w, res.Result)
}

// RenderEvent writes a single event in a compact one-line-ish form.
//
//	09:30:17 [INFO ] backup.started     deployment=db1 backup_id=...
//	  body: { "logical_bytes": 12345 }
//	  hint: <suggestion.human>
//	    run: <suggestion.command>
//	    docs: <suggestion.doc_url>
func (r *Renderer) RenderEvent(w io.Writer, ev *output.Event) error {
	if ev == nil {
		return nil
	}
	ts := ev.GeneratedAt.Format("15:04:05")
	header := fmt.Sprintf("%s [%-5s] %s.%s",
		ts, strings.ToUpper(ev.Severity.String()), ev.Component, ev.Op)
	if !ev.Subject.IsZero() {
		header += "  " + formatSubject(ev.Subject)
	}
	if _, err := fmt.Fprintln(w, header); err != nil {
		return err
	}
	if ev.Body != nil {
		if err := r.renderBodyIndented(w, "  body: ", ev.Body); err != nil {
			return err
		}
	}
	if ev.Suggestion != nil {
		if err := writeSuggestion(w, "  ", ev.Suggestion); err != nil {
			return err
		}
	}
	return nil
}

// renderError writes a structured error block.
func (r *Renderer) renderError(w io.Writer, e *output.Error) error {
	if e == nil {
		return nil
	}
	if _, err := fmt.Fprintf(w, "ERROR %s: %s\n", e.Code, e.Message); err != nil {
		return err
	}
	if !e.Subject.IsZero() {
		if _, err := fmt.Fprintf(w, "  subject: %s\n", formatSubject(e.Subject)); err != nil {
			return err
		}
	}
	if e.Suggestion != nil {
		if err := writeSuggestion(w, "  ", e.Suggestion); err != nil {
			return err
		}
	}
	return nil
}

// renderBody is the success-payload entry point.
func (r *Renderer) renderBody(w io.Writer, body any) error {
	switch b := body.(type) {
	case nil:
		return nil
	case string:
		_, err := fmt.Fprintln(w, b)
		return err
	case TextWriter:
		if err := b.WriteText(w); err != nil {
			return err
		}
		_, err := fmt.Fprintln(w)
		return err
	default:
		return marshalIndentedJSON(w, b)
	}
}

// renderBodyIndented nests body under a header line. The prefix is
// applied to the FIRST line of the rendered body; continuation lines
// are indented to align under the prefix's content (so multi-line
// JSON bodies hang neatly under their label rather than starting
// flush-left or with a different indent than callers asked for).
//
// For a prefix like "  body: ", a string body renders as:
//
//	body: hello
//
// and a JSON body renders as:
//
//	body: {
//	  "k": "v"
//	}
//
// where continuation lines are indented by len(prefix) so the JSON
// braces line up under the colon, matching what an operator scanning
// `pg_hardstorage logs -o text` expects.
func (r *Renderer) renderBodyIndented(w io.Writer, prefix string, body any) error {
	switch b := body.(type) {
	case nil:
		return nil
	case string:
		_, err := fmt.Fprintln(w, prefix+b)
		return err
	case TextWriter:
		// Typed body whose author committed to a compact text
		// rendering — issue #9's verbose backup events use
		// this so a per-file line stays one row instead of
		// expanding into a 5-line indented JSON block.
		buf := &strings.Builder{}
		if err := b.WriteText(buf); err != nil {
			return err
		}
		_, err := fmt.Fprintln(w, prefix+strings.TrimRight(buf.String(), "\n"))
		return err
	default:
		buf := &strings.Builder{}
		if err := marshalIndentedJSON(buf, b); err != nil {
			return err
		}
		lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
		// Continuation indent: spaces matching len(prefix) so
		// subsequent lines align under the prefix's content. This
		// keeps the visual structure of multi-line JSON intact.
		cont := strings.Repeat(" ", len(prefix))
		for i, line := range lines {
			head := cont
			if i == 0 {
				head = prefix
			}
			if _, err := fmt.Fprintln(w, head+line); err != nil {
				return err
			}
		}
		return nil
	}
}

func writeSuggestion(w io.Writer, indent string, s *output.Suggestion) error {
	if s == nil {
		return nil
	}
	if s.Human != "" {
		if _, err := fmt.Fprintf(w, "%shint: %s\n", indent, s.Human); err != nil {
			return err
		}
	}
	if s.Concept != "" {
		if _, err := fmt.Fprintf(w, "%s  why: %s\n", indent, s.Concept); err != nil {
			return err
		}
	}
	if s.Command != "" {
		if _, err := fmt.Fprintf(w, "%s  run: %s\n", indent, s.Command); err != nil {
			return err
		}
	}
	if s.DocURL != "" {
		if _, err := fmt.Fprintf(w, "%s  docs: %s\n", indent, s.DocURL); err != nil {
			return err
		}
	}
	return nil
}

func formatSubject(s output.Subject) string {
	parts := make([]string, 0, 5)
	if s.Tenant != "" {
		parts = append(parts, "tenant="+s.Tenant)
	}
	if s.Deployment != "" {
		parts = append(parts, "deployment="+s.Deployment)
	}
	if s.BackupID != "" {
		parts = append(parts, "backup_id="+s.BackupID)
	}
	if s.Timeline != 0 {
		parts = append(parts, fmt.Sprintf("timeline=%d", s.Timeline))
	}
	if s.LSN != "" {
		parts = append(parts, "lsn="+s.LSN)
	}
	return strings.Join(parts, " ")
}

func marshalIndentedJSON(w io.Writer, v any) error {
	enc := stdjson.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
