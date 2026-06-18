// Package ndjson renders one JSON document per line, no indentation.
//
// This is the streaming renderer: backup, restore, verify, wal stream,
// and logs all emit through it so consumers can pipe to jq / tee / sed
// and process records as they arrive.
//
// Note: for one-shot commands the json renderer is more readable; this
// package is fine for them too but produces a single long line.
package ndjson

import (
	stdjson "encoding/json"
	"io"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Name is the canonical name of this renderer.
const Name = "ndjson"

// Renderer writes one JSON document per line.
type Renderer struct{}

// New returns an NDJSON renderer.
func New() *Renderer { return &Renderer{} }

// Name implements output.Renderer.
func (r *Renderer) Name() string { return Name }

// RenderResult marshals the Result on a single line with a trailing newline.
func (r *Renderer) RenderResult(w io.Writer, res *output.Result) error {
	return marshalLine(w, res)
}

// RenderEvent marshals the Event on a single line with a trailing newline.
// One Write call per event so consumers see each record as it arrives.
func (r *Renderer) RenderEvent(w io.Writer, ev *output.Event) error {
	return marshalLine(w, ev)
}

// SupportsTTY reports false — NDJSON is for machine consumption.
func (r *Renderer) SupportsTTY() bool { return false }

// Close is a no-op.
func (r *Renderer) Close() error { return nil }

func marshalLine(w io.Writer, v any) error {
	// json.Encoder.Encode adds a trailing newline; that's what we want.
	enc := stdjson.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
