// Package yaml renders Result and Event values as YAML documents.
//
// Same schema as the JSON renderer (pg_hardstorage.v1) — operators
// who prefer YAML for human inspection get the same on-the-wire
// shape they'd get with `--output json`, just YAML-encoded.
//
// One YAML document per RenderResult / RenderEvent call.  Each
// document is preceded by `---` so concatenated output remains
// valid multi-doc YAML (`yq -s '.'` reads it back as a list).
//
// We use gopkg.in/yaml.v3 because the project already depends on
// it for config parsing — no new module deps.
package yaml

import (
	"fmt"
	"io"

	"gopkg.in/yaml.v3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Name is the canonical name of this renderer.
const Name = "yaml"

// Renderer is the YAML renderer.
type Renderer struct{}

// New returns a YAML renderer.
func New() *Renderer { return &Renderer{} }

// Name implements output.Renderer.
func (r *Renderer) Name() string { return Name }

// RenderResult marshals the Result as a YAML document.
func (r *Renderer) RenderResult(w io.Writer, res *output.Result) error {
	return marshalDoc(w, res)
}

// RenderEvent marshals the Event as a YAML document.
func (r *Renderer) RenderEvent(w io.Writer, ev *output.Event) error {
	return marshalDoc(w, ev)
}

// SupportsTTY reports false — YAML is structured output.
func (r *Renderer) SupportsTTY() bool { return false }

// Close is a no-op.
func (r *Renderer) Close() error { return nil }

func marshalDoc(w io.Writer, v any) error {
	// Round-trip through JSON shape so the YAML output uses the
	// same field names (and `omitempty` semantics) the rest of
	// the v1 schema commits to.  yaml.v3 reads json tags only
	// for plain `map[string]any`, so we pass through stdjson
	// first to get a plain map, then encode it as YAML.
	plain, err := jsonRoundTrip(v)
	if err != nil {
		return fmt.Errorf("yaml: round-trip via JSON: %w", err)
	}
	if _, err := io.WriteString(w, "---\n"); err != nil {
		return err
	}
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(plain); err != nil {
		return fmt.Errorf("yaml: encode: %w", err)
	}
	return enc.Close()
}
