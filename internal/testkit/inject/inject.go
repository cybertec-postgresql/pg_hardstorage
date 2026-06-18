// inject.go — Fault interface + registry for soak fault-injection primitives.
package inject

import (
	"context"
	"fmt"
	"sort"
)

// Fault is one named fault-injection primitive.
//
// Apply executes the fault against the supplied targets;
// the returned Recovery is invoked by the soak driver after
// a heal window to revert.  Some faults are inherently
// irreversible (signal sig=9 — the process is dead); those
// return NoRecovery.
type Fault interface {
	// Name matches the action prefix in the YAML catalogue
	// ("disk_full", "signal", ...).
	Name() string

	// Apply injects the fault.  args are the parsed key=value
	// pairs from the action string; targets is the soak fleet.
	// Returns a Recovery that the driver invokes (after a
	// heal window) to revert.
	Apply(ctx context.Context, args Args, targets TargetSet) (Recovery, error)
}

// Recovery reverts a previously-applied fault.  Faults that
// cannot revert (signals, byte flips already on disk) return
// NoRecovery.
type Recovery func(context.Context) error

// NoRecovery is what irreversible faults return.  Callable
// without a nil-check so the soak driver doesn't have to
// special-case it.
var NoRecovery Recovery = func(context.Context) error { return nil }

// Registry is the lookup table from action prefix → Fault.
// Faults register themselves via init() below; the soak
// driver calls Lookup(prefix) to dispatch parsed actions.
type Registry struct {
	faults map[string]Fault
}

// NewRegistry builds an empty registry.  Tests construct a
// registry directly to avoid touching the package-level
// DefaultRegistry.
func NewRegistry() *Registry {
	return &Registry{faults: map[string]Fault{}}
}

// Register adds a fault.  Panics on duplicate registration —
// a misconfigured init() should fail the binary at start, not
// silently shadow.
func (r *Registry) Register(f Fault) {
	if _, dup := r.faults[f.Name()]; dup {
		panic(fmt.Sprintf("inject: duplicate registration for %q", f.Name()))
	}
	r.faults[f.Name()] = f
}

// Lookup returns the fault for the given prefix, or an error
// listing every registered prefix.
func (r *Registry) Lookup(prefix string) (Fault, error) {
	if f, ok := r.faults[prefix]; ok {
		return f, nil
	}
	return nil, fmt.Errorf("inject: unknown fault prefix %q (registered: %s)",
		prefix, joinSorted(r.knownPrefixes()))
}

// Names returns every registered prefix, sorted, for display
// and validation.
func (r *Registry) Names() []string {
	out := r.knownPrefixes()
	sort.Strings(out)
	return out
}

func (r *Registry) knownPrefixes() []string {
	out := make([]string, 0, len(r.faults))
	for k := range r.faults {
		out = append(out, k)
	}
	return out
}

func joinSorted(items []string) string {
	sort.Strings(items)
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// Apply parses an action string, looks up the fault, and runs
// it.  Single entry-point used by the soak driver and by every
// test that goes through the registry.
func (r *Registry) Apply(ctx context.Context, action string, targets TargetSet) (Recovery, error) {
	pa, err := ParseAction(action)
	if err != nil {
		return nil, err
	}
	f, err := r.Lookup(pa.Prefix)
	if err != nil {
		return nil, err
	}
	return f.Apply(ctx, pa.Args, targets)
}

// DefaultRegistry holds every in-tree fault registered via
// init().  Production callers use this directly; tests
// construct their own.
var DefaultRegistry = NewRegistry()
