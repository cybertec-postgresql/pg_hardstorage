// router_executor.go — RouterExecutor: dispatches ControlPlaneJob to per-Kind JobExecutor.
package agent

import (
	"context"
	"fmt"
)

// RouterExecutor dispatches a ControlPlaneJob to a per-Kind
// JobExecutor. wires backup + restore; verify follows once the
// sandbox runner exposes a "verify a named backup" entry point.
//
// The router is intentionally dumb — no kind-specific argument
// parsing here, that lives in the per-Kind executor. This keeps each
// executor independently testable and lets a future plugin tier
// register additional kinds (e.g. logical-restore, partial-restore)
// without touching the router.
type RouterExecutor struct {
	executors map[string]JobExecutor
}

// NewRouterExecutor constructs a router with the supplied per-Kind
// map. Kinds not present in the map fall through to a structured
// "unknown kind" error, which the control plane records as a failed
// job (no silent successes).
func NewRouterExecutor(execs map[string]JobExecutor) *RouterExecutor {
	if execs == nil {
		execs = map[string]JobExecutor{}
	}
	return &RouterExecutor{executors: execs}
}

// Execute implements JobExecutor.
func (r *RouterExecutor) Execute(ctx context.Context, job *ControlPlaneJob, progress func(map[string]any)) (map[string]any, error) {
	if job == nil {
		return nil, fmt.Errorf("router-executor: nil job")
	}
	exec, ok := r.executors[job.Kind]
	if !ok {
		return nil, fmt.Errorf("router-executor: no executor registered for kind %q (registered: %v)",
			job.Kind, r.kinds())
	}
	return exec.Execute(ctx, job, progress)
}

// kinds returns the registered kinds in deterministic order. Used
// only by the error path so the operator can see what the agent will
// accept — small enough that the lack of caching is fine.
func (r *RouterExecutor) kinds() []string {
	out := make([]string, 0, len(r.executors))
	for k := range r.executors {
		out = append(out, k)
	}
	return out
}
