// Package template renders Result and Event values through a
// user-supplied Go text/template.
//
// Use case: monitoring scripts that want to extract one field
// without piping through `jq`.  Examples:
//
//	pg_hardstorage status -o template --template '{{(index .Result.deployments 0).rpo_seconds}}'
//	pg_hardstorage list db1 -o template --template '{{range .Result.backups}}{{.id}}{{"\n"}}{{end}}'
//
// The template is parsed once per renderer instance.  The data
// passed in is the JSON shape of the Result (after a stdjson
// round-trip into map[string]any), so field names match `--output
// json` exactly — operators write the same path against either
// renderer.  This keeps the `--template` UX honest: "what you'd
// pipe to jq" is "what you write here."
//
// We use text/template (not html/template) because the operator's
// terminal isn't an HTML context.  Template execution honours
// the Go stdlib's standard escaping rules; %% / { / } characters
// in field values aren't massaged.
package template

import (
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"text/template"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Name is the canonical name of this renderer.
const Name = "template"

// Renderer evaluates a Go text/template for each result/event.
type Renderer struct {
	tmpl *template.Template
}

// New parses templateText and returns a Renderer.  An empty
// templateText is rejected — operators who don't supply a
// template asked for the wrong renderer.
func New(templateText string) (*Renderer, error) {
	if templateText == "" {
		return nil, errors.New("template: empty template (set --template '<go text/template>')")
	}
	t, err := template.New("renderer").Parse(templateText)
	if err != nil {
		return nil, fmt.Errorf("template: parse: %w", err)
	}
	return &Renderer{tmpl: t}, nil
}

// Name implements output.Renderer.
func (r *Renderer) Name() string { return Name }

// RenderResult executes the template against the Result.
func (r *Renderer) RenderResult(w io.Writer, res *output.Result) error {
	return r.execute(w, res)
}

// RenderEvent executes the template against the Event.
func (r *Renderer) RenderEvent(w io.Writer, ev *output.Event) error {
	return r.execute(w, ev)
}

// SupportsTTY reports true — operators sometimes use templates
// to render summary lines on a terminal.  The renderer doesn't
// emit ANSI colour, but it doesn't need to.
func (r *Renderer) SupportsTTY() bool { return true }

// Close is a no-op.
func (r *Renderer) Close() error { return nil }

func (r *Renderer) execute(w io.Writer, v any) error {
	plain, err := jsonRoundTrip(v)
	if err != nil {
		return fmt.Errorf("template: round-trip via JSON: %w", err)
	}
	if err := r.tmpl.Execute(w, plain); err != nil {
		return fmt.Errorf("template: execute: %w", err)
	}
	return nil
}

func jsonRoundTrip(v any) (any, error) {
	b, err := stdjson.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := stdjson.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}
