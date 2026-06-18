// sinkspec.go — SinkSpec: declarative YAML form of a sink config resolved through SinkRegistry.
package output

import (
	"errors"
	"fmt"
)

// SinkSpec is the declarative form of a sink configuration. It comes
// from YAML — see internal/config — and is resolved at startup to a
// concrete Sink via the global SinkRegistry.
//
// Plugin selects which Tier-1 implementation handles this spec; Config
// is the plugin-specific bag of settings (webhook URL, syslog address,
// etc.). Name is the operator-chosen label that goes on every emitted
// event for diagnostics.
type SinkSpec struct {
	// Name is the operator's label for this sink ("ops-slack",
	// "audit-cef"). Surfaces in events the dispatcher emits about
	// the sink itself (open errors, panic recovery, etc.).
	Name string `yaml:"name" json:"name"`

	// Plugin is the lowercase plugin identifier ("slack", "webhook",
	// "syslog"). Looked up in the global SinkRegistry at start.
	Plugin string `yaml:"plugin" json:"plugin"`

	// Config is the plugin-specific configuration. Keys are
	// per-plugin and documented in the plugin's package; common keys
	// across plugins ("min_severity") are documented on FilterConfig.
	Config map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
}

// SinkBuilder constructs a Sink from a SinkSpec. The function returns
// (sink, err); err is non-nil for invalid configuration. Sinks are
// expected to be ready-to-Emit on return — Open is called by the
// dispatcher right before the first Emit, so Open and the builder
// together cover the lifecycle.
type SinkBuilder func(spec SinkSpec) (Sink, error)

// SinkRegistry is the lookup table from plugin name to builder.
//
// We intentionally do NOT import any concrete sink package here —
// each plugin self-registers in init(), so a binary that doesn't
// link a plugin doesn't pay for it.
type SinkRegistry struct {
	builders map[string]SinkBuilder
}

// NewSinkRegistry returns an empty registry.
func NewSinkRegistry() *SinkRegistry {
	return &SinkRegistry{builders: map[string]SinkBuilder{}}
}

// Register installs builder under plugin. Panics on double-registration:
// the registry is initialised once at process start; double-register
// is a programmer error.
func (r *SinkRegistry) Register(plugin string, builder SinkBuilder) {
	if _, ok := r.builders[plugin]; ok {
		panic(fmt.Sprintf("output: sink plugin %q already registered", plugin))
	}
	r.builders[plugin] = builder
}

// Plugins returns the list of registered plugin names. Useful for
// "supported plugins:" error messages.
func (r *SinkRegistry) Plugins() []string {
	out := make([]string, 0, len(r.builders))
	for k := range r.builders {
		out = append(out, k)
	}
	return out
}

// Build resolves spec to a Sink. Returns ErrUnknownSinkPlugin when
// the plugin isn't registered.
func (r *SinkRegistry) Build(spec SinkSpec) (Sink, error) {
	if spec.Plugin == "" {
		return nil, errors.New("output: SinkSpec.Plugin is required")
	}
	if spec.Name == "" {
		return nil, errors.New("output: SinkSpec.Name is required")
	}
	b, ok := r.builders[spec.Plugin]
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %v)",
			ErrUnknownSinkPlugin, spec.Plugin, r.Plugins())
	}
	return b(spec)
}

// BuildAll resolves every spec, collecting per-spec failures into a
// slice the caller can render as warning events. The successfully-
// built sinks are returned alongside; one failure does not stop the
// rest.
//
// This shape — partial success with structured per-item errors — is
// the right posture for a startup-time configuration phase: the
// operator might have one bad sink config alongside several good
// ones, and we want the good ones to keep working while the bad one
// surfaces a clear remediation.
func (r *SinkRegistry) BuildAll(specs []SinkSpec) (sinks []Sink, errs []SinkBuildError) {
	for _, spec := range specs {
		s, err := r.Build(spec)
		if err != nil {
			errs = append(errs, SinkBuildError{Spec: spec, Err: err})
			continue
		}
		sinks = append(sinks, s)
	}
	return sinks, errs
}

// SinkBuildError pairs a failing SinkSpec with the builder error.
// Render-friendly: tests and CLI both walk the slice and emit one
// warning event per entry.
type SinkBuildError struct {
	Spec SinkSpec
	Err  error
}

// Error implements error.
func (e SinkBuildError) Error() string {
	return fmt.Sprintf("sink %q (plugin %s): %v", e.Spec.Name, e.Spec.Plugin, e.Err)
}

// Unwrap exposes the inner error for errors.Is / errors.As walks.
func (e SinkBuildError) Unwrap() error { return e.Err }

// ErrUnknownSinkPlugin is returned when SinkSpec.Plugin doesn't map
// to a registered builder.
var ErrUnknownSinkPlugin = errors.New("output: unknown sink plugin")

// DefaultSinkRegistry is the process-wide registry every plugin
// init() registers against. The dispatcher constructor consults this
// registry by default; tests build their own via NewSinkRegistry.
var DefaultSinkRegistry = NewSinkRegistry()

// SinkConfigString reads cfg[key] as a string. Returns "" if key is
// absent. Errors when the value is set but not a string — that's a
// plain config-typing mistake the operator should hear about.
func SinkConfigString(cfg map[string]any, key string) (string, error) {
	v, ok := cfg[key]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("config key %q: expected string, got %T", key, v)
	}
	return s, nil
}

// SinkConfigStringDefault is SinkConfigString with a default value
// for the absent case. Same error semantics for present-but-wrong-type.
func SinkConfigStringDefault(cfg map[string]any, key, def string) (string, error) {
	v, err := SinkConfigString(cfg, key)
	if err != nil {
		return "", err
	}
	if v == "" {
		return def, nil
	}
	return v, nil
}
