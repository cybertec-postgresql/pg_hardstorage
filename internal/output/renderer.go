// renderer.go — Renderer interface: synchronous per-invocation output plugin (text/json/ndjson/etc).
package output

import "io"

// Renderer is the synchronous, command-scoped output plugin tier.
//
// A Renderer takes typed values (Result for one-shot commands, Event for
// streaming commands) and writes bytes to a Writer. The dispatcher picks
// exactly one Renderer per CLI invocation based on --output / env /
// TTY auto-detection.
//
// Implementations must be safe to call from a single goroutine; the
// dispatcher serializes calls. They must NOT internally wrap the writer
// in a buffered writer that delays output, because streaming commands
// (NDJSON) rely on per-event flushing.
//
// Note for plugin authors: unlike the other six plugin tiers (Storage,
// Source, Encryption, Compression, Sink, LLMProvider), Renderer
// deliberately has no DefaultRegistry / Schemes() lookup function.
// Renderers are picked by --output value at command construction
// time and there is exactly one renderer per CLI invocation, so the
// registry ceremony adds nothing.  The dispatcher in `dispatcher.go`
// builds renderers by name via a small switch; plugin authors adding
// a Tier-1 renderer extend that switch (and the renderers/ subdirs).
// Tier-2 renderers via `go-plugin` are not supported — the
// surface is small enough that in-tree extension is the right
// posture.
type Renderer interface {
	// Name returns the canonical lowercase name (e.g. "text", "json").
	Name() string

	// RenderResult writes a one-shot Result. Called exactly once per
	// non-streaming command. Implementations should write a trailing
	// newline so terminal sessions are tidy.
	RenderResult(w io.Writer, r *Result) error

	// RenderEvent writes a single streaming Event. Called many times.
	// Each invocation should write exactly one logical record (one line
	// for line-oriented renderers, one paragraph for text).
	RenderEvent(w io.Writer, e *Event) error

	// SupportsTTY reports whether this renderer is appropriate for an
	// interactive terminal. The text renderer says true; structured
	// renderers say false. Used by TTY auto-detection.
	SupportsTTY() bool

	// Close releases any renderer-side resources. The dispatcher calls
	// it once at the end of the CLI invocation.
	Close() error
}
