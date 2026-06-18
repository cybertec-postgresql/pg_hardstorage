// Package json renders Result and Event values as indented JSON.
//
// One JSON document per RenderResult call; one JSON document per
// RenderEvent call. Use ndjson for streaming commands; this renderer is
// for one-shot commands and pretty inspection.
package json

import (
	stdjson "encoding/json"
	"io"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Name is the canonical name of this renderer.
const Name = "json"

// Renderer is the indented-JSON renderer.
type Renderer struct{}

// New returns a JSON renderer.
func New() *Renderer { return &Renderer{} }

// Name implements output.Renderer.
func (r *Renderer) Name() string { return Name }

// RenderResult marshals the Result with two-space indentation.
func (r *Renderer) RenderResult(w io.Writer, res *output.Result) error {
	return marshalIndented(w, res)
}

// RenderEvent marshals the Event with two-space indentation. For high-rate
// streaming, prefer the ndjson renderer.
func (r *Renderer) RenderEvent(w io.Writer, ev *output.Event) error {
	return marshalIndented(w, ev)
}

// SupportsTTY reports false — JSON is for machine consumption.
func (r *Renderer) SupportsTTY() bool { return false }

// Close is a no-op.
func (r *Renderer) Close() error { return nil }

func marshalIndented(w io.Writer, v any) error {
	enc := stdjson.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
