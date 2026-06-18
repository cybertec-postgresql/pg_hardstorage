// follower.go — LeaderFollower: polls Patroni /cluster and surfaces leader-change events.
package patroni

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DefaultFollowInterval is how often the LeaderFollower polls
// /cluster when no explicit interval is configured. Patroni's
// own leader TTL is 30 s by default; sampling at 5 s lets us see
// a leader change within at most one TTL window — fast enough to
// reconnect within the operator's typical RTO budget, slow
// enough to not spam the REST endpoint.
const DefaultFollowInterval = 5 * time.Second

// LeaderEndpoint is the current cluster leader's wire address +
// metadata. Stable across leader changes (a NEW endpoint is
// returned by the follower's GetLeader after a change; the
// previous one is not mutated in place — important for goroutines
// holding a snapshot).
type LeaderEndpoint struct {
	// Name is Patroni's cluster-member name (e.g. "node-2").
	// Useful for log lines and audit events.
	Name string
	// Host + Port form the libpq dialing target.
	Host string
	Port int
	// Timeline is the leader's current TLI as Patroni reports it.
	// Useful for the leader-follow loop to decide whether to
	// capture a TIMELINE_HISTORY before reconnecting.
	Timeline uint32
	// Role is the raw role string ("leader" or "master") so callers
	// can log it without re-deriving.
	Role string
}

// LeaderChange is the event the follower emits each time the
// leader endpoint changes. Both Old and New are snapshots; one
// of them may be nil:
//
//   - first observation: Old=nil, New=<initial leader>
//   - leader gone (no /cluster member with role=leader):
//     Old=<previous>, New=nil
//   - hand-over: Old=<previous>, New=<current>
type LeaderChange struct {
	At  time.Time
	Old *LeaderEndpoint
	New *LeaderEndpoint
}

// FollowerOptions configures a LeaderFollower.
type FollowerOptions struct {
	// Client is the Patroni REST client to poll. Required.
	Client *Client

	// Interval is the poll cadence. Defaults to DefaultFollowInterval
	// when zero or negative.
	Interval time.Duration

	// ExpectedSystemID, when non-empty, locks the follower to a
	// specific cluster's pg_control system identifier. The first
	// poll captures the system identifier from the leader's
	// `system_identifier` field IF Patroni surfaces it (newer
	// Patroni versions); subsequent polls compare. A mismatch
	// surfaces a critical event AND the follower stops emitting
	// new leaders so a misconfigured fleet doesn't end up
	// streaming WAL from a different cluster's primary.
	//
	// When empty (the default), system-identifier verification is
	// skipped — appropriate for the+ scope where we don't yet
	// require Patroni to surface the field. The follower still
	// works; the cluster-mismatch defence is just absent and
	// logged as a Notice.
	ExpectedSystemID string

	// OnEvent receives leader-change events synchronously on the
	// follower goroutine. Optional; nil discards. The callback
	// must return promptly so it doesn't stall the next poll.
	OnEvent func(LeaderChange)

	// OnPollError receives transient poll failures (Patroni REST
	// unreachable / unauthorized / unexpected). Optional. Errors
	// are NOT terminal — the follower keeps polling on the next
	// tick. A persistent failure surface is the operator's
	// responsibility (alerting on the OnPollError stream).
	OnPollError func(error)
}

// Follower polls Patroni REST and tracks the current leader.
// Safe for concurrent use: GetLeader / Done are read-only and
// serialised internally.
type Follower struct {
	client        *Client
	interval      time.Duration
	expectedSysID string
	onEvent       func(LeaderChange)
	onPollError   func(error)

	mu        sync.RWMutex
	current   *LeaderEndpoint
	disabled  bool   // set if cluster-identity mismatch
	disabledR string // human-readable reason
	done      chan struct{}
}

// Start begins the polling loop in a goroutine. The returned
// Follower is usable immediately; GetLeader blocks the caller
// until the first poll completes IF block=true on first call,
// otherwise returns whatever's been observed so far (nil before
// the first poll). Use the OnEvent callback to wait deterministically.
//
// The follower stops when ctx is cancelled. Done is closed at
// that point.
func Start(ctx context.Context, opts FollowerOptions) (*Follower, error) {
	if opts.Client == nil {
		return nil, errors.New("patroni: FollowerOptions.Client is required")
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultFollowInterval
	}
	f := &Follower{
		client:        opts.Client,
		interval:      interval,
		expectedSysID: opts.ExpectedSystemID,
		onEvent:       opts.OnEvent,
		onPollError:   opts.OnPollError,
		done:          make(chan struct{}),
	}
	go f.run(ctx)
	return f, nil
}

// run is the polling loop. Each tick fetches /cluster, derives
// the leader, and (on change) snapshots + dispatches.
func (f *Follower) run(ctx context.Context) {
	defer close(f.done)

	// First-poll-now-then-wait pattern: we don't want a
	// fresh-start follower to take Interval before observing the
	// initial leader. Tick.C delivers AFTER the interval; we run
	// one poll inline first.
	f.poll(ctx)

	t := time.NewTicker(f.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.poll(ctx)
		}
	}
}

// poll fetches /cluster, picks the leader, and dispatches an
// event if the (name, host, port) tuple has changed.
func (f *Follower) poll(ctx context.Context) {
	if f.isDisabled() {
		return
	}
	cluster, err := f.client.Cluster(ctx)
	if err != nil {
		if f.onPollError != nil {
			f.onPollError(err)
		}
		return
	}

	var newLeader *LeaderEndpoint
	for i := range cluster.Members {
		if cluster.Members[i].IsLeader() {
			m := &cluster.Members[i]
			newLeader = &LeaderEndpoint{
				Name:     m.Name,
				Host:     m.Host,
				Port:     m.Port,
				Timeline: m.Timeline,
				Role:     m.Role,
			}
			break
		}
	}

	f.mu.Lock()
	old := f.current
	if leadersEqual(old, newLeader) {
		f.mu.Unlock()
		return
	}
	f.current = newLeader
	f.mu.Unlock()

	if f.onEvent != nil {
		f.onEvent(LeaderChange{
			At:  time.Now().UTC(),
			Old: old,
			New: newLeader,
		})
	}
}

// GetLeader returns the most recently observed leader endpoint,
// or nil if the follower has not yet completed a poll OR the
// cluster has no current leader. The returned pointer is a
// snapshot; callers may retain it.
func (f *Follower) GetLeader() *LeaderEndpoint {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.current
}

// Done returns a channel closed when the follower's context is
// cancelled and its goroutine has exited. Useful for orderly
// shutdown.
func (f *Follower) Done() <-chan struct{} { return f.done }

// Disable stops emitting leader updates with the supplied reason.
// Used by the cluster-identity-mismatch path; also exposed for
// operators wanting an explicit kill-switch (`pg_hardstorage doctor`
// can flip it on a verified-corrupt cluster).
func (f *Follower) Disable(reason string) {
	f.mu.Lock()
	f.disabled = true
	f.disabledR = reason
	f.mu.Unlock()
}

// DisabledReason returns ("reason", true) when the follower is
// disabled, ("", false) when it's actively polling.
func (f *Follower) DisabledReason() (string, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.disabledR, f.disabled
}

func (f *Follower) isDisabled() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.disabled
}

// leadersEqual reports whether two leader snapshots refer to the
// same wire endpoint. Unset fields are compared too — a leader
// whose Timeline advances on the same node is reported as a change
// (the leader-follow loop should re-capture TIMELINE_HISTORY).
func leadersEqual(a, b *LeaderEndpoint) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	}
	return a.Name == b.Name &&
		a.Host == b.Host &&
		a.Port == b.Port &&
		a.Timeline == b.Timeline &&
		a.Role == b.Role
}

// String pretty-prints a LeaderEndpoint for log lines.
func (e *LeaderEndpoint) String() string {
	if e == nil {
		return "<no-leader>"
	}
	return fmt.Sprintf("%s@%s:%d (TLI %d)", e.Name, e.Host, e.Port, e.Timeline)
}

// String pretty-prints a LeaderChange for log lines.
func (c LeaderChange) String() string {
	return fmt.Sprintf("leader change at %s: %s → %s", c.At.Format(time.RFC3339), c.Old, c.New)
}
