// Package tools defines the LLM tool surface — the small set of
// read-only and preview operations a skill may invoke.
//
// Each Tool is named, declares its arg schema, and carries a Run
// function that returns either a JSON-serialisable result or an
// error. The orchestrator passes ToolDefs to the provider; when the
// provider invokes a tool, the orchestrator looks up the matching
// Tool here and calls Run.
//
// v0.1 ships a minimum useful set:
//
//	read_doctor          — call doctor.Run(deployment?) and return it
//	read_status          — equivalent of `pg_hardstorage status`
//	read_runbook         — load a shipped runbook by ID (R1..R7)
//	preview_command      — render a `--preview` for a CLI invocation
//	suggest_command      — record a suggestion (no execution)
//
// What's deferred to: search_docs (semantic search), search_fleet,
// read_audit (RBAC-scoped), read_logs.
//
// What's deliberately NOT in v0.1: execute_command. The whole skill
// surface ships read-only; advise+execute lands alongside the
// confirmation/preview-replay-protected dispatch the SPEC describes.
package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Tool is one named capability. Args is a map[string]any populated
// from the model's tool-call invocation; Run returns a Result or an
// error. The orchestrator marshals Result back to the model.
type Tool interface {
	// Name is the tool's stable identifier ("read_doctor").
	Name() string

	// Description is the human-readable summary the provider uses
	// to decide whether/when to invoke. Short imperative sentence.
	Description() string

	// Schema returns a JSON-Schema-shaped description of the args.
	// v0.1 returns a small mapping; may grow to full JSON
	// Schema once providers consume it.
	Schema() map[string]any

	// Run invokes the tool. ctx cancellation aborts; long-running
	// tools must honour it.
	Run(ctx context.Context, args map[string]any) (Result, error)

	// ReadOnly reports whether the tool mutates state. v0.1 is
	// read-only-only; this getter exists to enforce that at the
	// orchestrator and to gate's execute_command behind a
	// runtime mode check.
	ReadOnly() bool
}

// Result is the structured tool output. Body is JSON-serialisable.
// Summary is a short human-readable line the orchestrator can
// surface to the user without the full body.
type Result struct {
	Summary string `json:"summary,omitempty"`
	Body    any    `json:"body,omitempty"`
}

// Registry maps tool names to instances. Tools self-register via
// init() against DefaultRegistry.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register adds t to the registry. Re-registering the same name
// overwrites — intentional for operator overrides via Tier-2
// plugins.
func (r *Registry) Register(t Tool) {
	if t == nil || t.Name() == "" {
		panic("tools: Register requires a non-nil Tool with a non-empty Name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
}

// Get returns the named tool or ErrUnknownTool.
func (r *Registry) Get(name string) (Tool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTool, name)
	}
	return t, nil
}

// All returns every registered tool, sorted by name.
func (r *Registry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Filter returns the subset of registered tools whose names appear in
// allow. Used by the orchestrator to honour each skill's
// available_tools allow-list.
func (r *Registry) Filter(allow []string) []Tool {
	wanted := map[string]struct{}{}
	for _, n := range allow {
		wanted[n] = struct{}{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(allow))
	for _, t := range r.tools {
		if _, ok := wanted[t.Name()]; ok {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// ErrUnknownTool is returned by Get when the name isn't registered.
var ErrUnknownTool = errors.New("tools: unknown tool")

// DefaultRegistry is the package-level registry every in-tree tool
// self-registers against.
var DefaultRegistry = NewRegistry()
