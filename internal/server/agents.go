// agents.go — in-memory agent registry: registration, heartbeat,
// active-window filtering for the control-plane fleet view.

package server

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// AgentRegistry is the in-memory agent map. Agents register at
// startup, heartbeat every ~10s, and fall out of the "active" list
// when they miss two heartbeats (HeartbeatTimeout window).
//
// Concurrency: every public method takes mu.RLock or Lock as
// appropriate. The map is small (one entry per agent host) so even
// a short critical section under heavy heartbeat load is fine.
type AgentRegistry struct {
	mu      sync.RWMutex
	timeout time.Duration
	agents  map[string]*Agent
}

// Agent is one registered agent instance.
type Agent struct {
	ID            string    `json:"id"`
	Host          string    `json:"host"`
	Version       string    `json:"version,omitempty"`
	Deployments   []string  `json:"deployments,omitempty"`
	RegisteredAt  time.Time `json:"registered_at"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

// IsActive reports whether the agent's last heartbeat falls within
// the registry's timeout window.
func (a *Agent) IsActive(timeout time.Duration, now time.Time) bool {
	return now.Sub(a.LastHeartbeat) <= timeout
}

// NewAgentRegistry returns an empty registry. timeout is the
// inactive cutoff (zero defaults to 30s).
func NewAgentRegistry(timeout time.Duration) *AgentRegistry {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &AgentRegistry{timeout: timeout, agents: map[string]*Agent{}}
}

// HeartbeatRequest is the body of POST /v1/agents/heartbeat. The
// agent posts itself in: ID + host + (optional) deployments + version.
type HeartbeatRequest struct {
	ID          string   `json:"id"`
	Host        string   `json:"host"`
	Version     string   `json:"version,omitempty"`
	Deployments []string `json:"deployments,omitempty"`
}

// Heartbeat upserts the agent record. First-time IDs are recorded
// with RegisteredAt = now; subsequent calls update LastHeartbeat
// (and Deployments / Version, which can change at agent-restart).
func (r *AgentRegistry) Heartbeat(req HeartbeatRequest) (*Agent, error) {
	if err := validateHeartbeat(req); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	a, ok := r.agents[req.ID]
	if !ok {
		a = &Agent{
			ID:           req.ID,
			RegisteredAt: now,
		}
		r.agents[req.ID] = a
	}
	a.Host = req.Host
	a.Version = req.Version
	// Allocate a fresh backing array on every heartbeat instead of
	// reusing the old one (append(a.Deployments[:0], ...)): snapshots
	// handed out by Heartbeat/List/Get share the slice header they were
	// copied from, and rewriting the old array in place raced with
	// readers iterating those snapshots after the lock was released
	// (concurrency audit, bug A).
	a.Deployments = cloneDeployments(req.Deployments)
	a.LastHeartbeat = now
	out := snapshotAgent(a)
	return &out, nil
}

// snapshotAgent returns a copy of a that shares no mutable state with
// the registry-owned record. Caller must hold r.mu (read or write).
// The Deployments deep copy is defense in depth on top of Heartbeat's
// fresh-slice allocation: no snapshot ever aliases registry memory.
func snapshotAgent(a *Agent) Agent {
	out := *a
	out.Deployments = cloneDeployments(a.Deployments)
	return out
}

// cloneDeployments deep-copies a deployments slice. Empty input maps
// to nil so the JSON omitempty semantics are preserved.
func cloneDeployments(d []string) []string {
	if len(d) == 0 {
		return nil
	}
	return append([]string(nil), d...)
}

// List returns every registered agent. includeInactive=false omits
// agents whose last heartbeat is past the timeout. Sorted by ID for
// stable output.
func (r *AgentRegistry) List(includeInactive bool) []Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	now := time.Now().UTC()
	out := make([]Agent, 0, len(r.agents))
	for _, a := range r.agents {
		if !includeInactive && !a.IsActive(r.timeout, now) {
			continue
		}
		out = append(out, snapshotAgent(a))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get returns the named agent, or nil if not registered.
func (r *AgentRegistry) Get(id string) *Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.agents[id]
	if !ok {
		return nil
	}
	out := snapshotAgent(a)
	return &out
}

// Remove drops the agent. Used by graceful-shutdown handlers (when
// they land) and by tests; the registry doesn't auto-prune
// expired entries — the inactive-filter at List time is sufficient
// for v0.1.
func (r *AgentRegistry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.agents, id)
}

// Timeout returns the configured inactive threshold.
func (r *AgentRegistry) Timeout() time.Duration { return r.timeout }

// maxAgentFieldLen bounds each heartbeat string field. Agent-supplied
// values (id, host, version, deployment names) are stored and echoed in
// /v1/agents; without a bound an authenticated agent could bloat the
// registry/responses, and control characters could corrupt logs/output
// (input-validation audit #5).
const maxAgentFieldLen = 256

// validateHeartbeat enforces required-ness, a length bound, and a
// no-control-character rule on every agent-supplied heartbeat field.
func validateHeartbeat(req HeartbeatRequest) error {
	if err := validateAgentField("id", req.ID, true); err != nil {
		return err
	}
	if err := validateAgentField("host", req.Host, true); err != nil {
		return err
	}
	if err := validateAgentField("version", req.Version, false); err != nil {
		return err
	}
	for _, d := range req.Deployments {
		if err := validateAgentField("deployment", d, true); err != nil {
			return err
		}
	}
	return nil
}

func validateAgentField(name, v string, required bool) error {
	if v == "" {
		if required {
			return errBadHeartbeat("agents: " + name + " is required")
		}
		return nil
	}
	if len(v) > maxAgentFieldLen {
		return errBadHeartbeat(fmt.Sprintf("agents: %s exceeds the %d-byte limit", name, maxAgentFieldLen))
	}
	if i := strings.IndexFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }); i >= 0 {
		return errBadHeartbeat(fmt.Sprintf("agents: %s contains a control character", name))
	}
	return nil
}

// errBadHeartbeat is a sentinel for handler-level usage-error mapping.
type errBadHeartbeat string

// Error returns the diagnostic message verbatim. Implements the
// error interface so errBadHeartbeat values can be returned from
// handler-internal validation.
func (e errBadHeartbeat) Error() string { return string(e) }
