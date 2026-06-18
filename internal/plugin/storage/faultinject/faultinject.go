// Package faultinject implements a fault-injecting middleware that
// wraps a StoragePlugin and selectively returns errors to its
// callers. The plan calls this out under the "automated game days"
// resilience section: "S3 503 storms (via a built-in fault-
// injection middleware)" plus the testkit's per-key per-time-window
// fault-rule shape.
//
// Two concrete uses:
//
//   - **Gameday `s3_throttle` runtime drive.** The scenario wraps
//     the configured backend in a Middleware and runs an operation
//     under the fault to demonstrate the system's recovery.
//
//   - **Testkit fault drills.** Tests construct a Middleware around
//     a real backend and assert the system's retry / circuit-breaker
//     behaviour under controlled fault patterns.
//
// Design:
//
//   - **Rule** declares a fault: which Ops, which key prefix, which
//     error to return, optional MaxFires (numeric cap), optional
//     ActiveUntil (time-bounded fault window). The first rule
//     whose conditions match wins; non-matching ops fall through
//     to the inner plugin.
//
//   - **Middleware** holds an ordered rule list and a hit counter.
//     `Activate(rules, opts)` swaps in a new rule set and resets
//     counters; `Deactivate()` clears the rules; `Stats()` reports
//     per-rule hit counts.
//
//   - **No global state.** Every Middleware instance is independent
//     so concurrent gameday runs (or testkit runs) don't interfere.
//     The wrapper itself is goroutine-safe — a sync.RWMutex guards
//     the rule list and counters.
//
// What this is NOT: a network fault simulator. We don't simulate
// latency, partial reads, or TCP-level weirdness. We return a
// configured Go error from the StoragePlugin method. That's enough
// to drive the retry-budget + circuit-breaker behaviour we care
// about for resilience verification.
package faultinject

import (
	"context"
	"errors"
	"io"
	"iter"
	"strings"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Op is a bitmask selecting which StoragePlugin methods a Rule
// applies to. AllOps is the union; OpPut is the most common in
// practice.
type Op uint16

const (
	// OpPut selects StoragePlugin.Put.
	OpPut Op = 1 << iota
	// OpGet selects StoragePlugin.Get.
	OpGet
	// OpStat selects StoragePlugin.Stat.
	OpStat
	// OpList selects StoragePlugin.List.
	OpList
	// OpDelete selects StoragePlugin.Delete.
	OpDelete
	// OpRename selects StoragePlugin.RenameIfNotExists.
	OpRename
	// OpSetRetention selects StoragePlugin.SetRetention.
	OpSetRetention
)

// AllOps is the union of every method the wrapper meters.
const AllOps Op = OpPut | OpGet | OpStat | OpList | OpDelete | OpRename | OpSetRetention

// HasOp reports whether the bitmask includes the named Op.
func (o Op) HasOp(other Op) bool { return o&other != 0 }

// String renders the bitmask as a comma-separated list of op names.
// Order is stable (Put,Get,Stat,List,Delete,Rename,SetRetention).
func (o Op) String() string {
	if o == 0 {
		return "none"
	}
	if o == AllOps {
		return "all"
	}
	parts := []string{}
	for _, named := range []struct {
		bit  Op
		name string
	}{
		{OpPut, "Put"},
		{OpGet, "Get"},
		{OpStat, "Stat"},
		{OpList, "List"},
		{OpDelete, "Delete"},
		{OpRename, "Rename"},
		{OpSetRetention, "SetRetention"},
	} {
		if o.HasOp(named.bit) {
			parts = append(parts, named.name)
		}
	}
	return strings.Join(parts, ",")
}

// Rule declares one fault-injection condition.
//
// Matching: a request matches the rule iff
//
//   - `Ops & op != 0` (the op is selected), AND
//   - `KeyPrefix == ""` OR the request's key has that prefix, AND
//   - `MaxFires == 0` OR the rule has fired fewer times.
//
// The currently-active time window is the wider Middleware's
// ActiveUntil; per-rule expiry isn't a thing today (operators
// configure the whole Middleware as a fault episode).
type Rule struct {
	Name      string // human-readable label, surfaced in Stats
	Ops       Op     // which methods this rule fires for
	KeyPrefix string // empty = match any key
	Err       error  // the error to return from the matched method
	MaxFires  int    // 0 = unlimited
}

// Middleware wraps a StoragePlugin and conditionally short-circuits
// requests with errors per its active rule set.
type Middleware struct {
	inner storage.StoragePlugin

	mu          sync.RWMutex
	rules       []Rule
	hits        []int
	activeUntil time.Time

	nowFn func() time.Time // overridable in tests
}

// New wraps inner. The wrapper is initially inactive (no rules,
// every request passes through unchanged). Call Activate to install
// rules.
func New(inner storage.StoragePlugin) *Middleware {
	return &Middleware{
		inner: inner,
		nowFn: time.Now,
	}
}

// WithClock overrides the wrapper's time source. Used by tests to
// drive deterministic ActiveUntil expiry. Must be called before
// Activate; switching the clock under a live fault is undefined.
func (m *Middleware) WithClock(now func() time.Time) *Middleware {
	m.nowFn = now
	return m
}

// ActivateOptions tunes Activate.
type ActivateOptions struct {
	// ActiveDuration bounds the fault window. Zero means "until
	// Deactivate()". Most gameday scenarios pass a non-zero value
	// so a forgotten Deactivate doesn't leave an injection live
	// indefinitely.
	ActiveDuration time.Duration
}

// Activate installs the rule set. Counters and the active-until
// clock reset to fresh state. Pass an empty rules slice to disable
// (equivalent to Deactivate).
func (m *Middleware) Activate(rules []Rule, opts ActivateOptions) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules = append([]Rule(nil), rules...)
	m.hits = make([]int, len(rules))
	if opts.ActiveDuration > 0 {
		m.activeUntil = m.nowFn().Add(opts.ActiveDuration)
	} else {
		m.activeUntil = time.Time{}
	}
}

// Deactivate clears the rule set. After this call, every request
// passes through unchanged.
func (m *Middleware) Deactivate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules = nil
	m.hits = nil
	m.activeUntil = time.Time{}
}

// IsActive reports whether the wrapper currently has live rules.
// Time-based fault windows that have expired are reported as
// inactive even if Deactivate hasn't been called yet.
func (m *Middleware) IsActive() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.isActiveLocked()
}

func (m *Middleware) isActiveLocked() bool {
	if len(m.rules) == 0 {
		return false
	}
	if m.activeUntil.IsZero() {
		return true
	}
	return m.nowFn().Before(m.activeUntil)
}

// RuleStat is one entry in Stats's return.
type RuleStat struct {
	Name string `json:"name"`
	Ops  string `json:"ops"`
	Hits int    `json:"hits"`
}

// Stats returns per-rule hit counts. Useful for gameday Evidence
// + testkit assertions ("did rule X actually fire?").
func (m *Middleware) Stats() []RuleStat {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]RuleStat, len(m.rules))
	for i, r := range m.rules {
		out[i] = RuleStat{
			Name: r.Name,
			Ops:  r.Ops.String(),
			Hits: m.hits[i],
		}
	}
	return out
}

// matchAndRecord finds the first rule that matches (op, key) and
// returns its error after incrementing its counter. Returns nil
// when no rule matches. Callers MUST treat a non-nil return as the
// authoritative response — the inner plugin is never consulted.
func (m *Middleware) matchAndRecord(op Op, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.isActiveLocked() {
		return nil
	}
	for i, r := range m.rules {
		if !r.Ops.HasOp(op) {
			continue
		}
		if r.KeyPrefix != "" && !strings.HasPrefix(key, r.KeyPrefix) {
			continue
		}
		if r.MaxFires > 0 && m.hits[i] >= r.MaxFires {
			continue
		}
		m.hits[i]++
		return r.Err
	}
	return nil
}

// --- StoragePlugin pass-through methods -----------------------------

// Name returns the inner plugin's name; the wrapper is transparent.
func (m *Middleware) Name() string { return m.inner.Name() }

// Open delegates. Fault rules don't affect Open — that would make
// the wrapper unusable.
func (m *Middleware) Open(ctx context.Context, cfg storage.StorageConfig) error {
	return m.inner.Open(ctx, cfg)
}

// Put consults the rule list before delegating. A matching rule
// short-circuits the call with the configured error; the body
// reader is NOT drained (the caller's bytes are discarded the same
// way a real backend reject would).
func (m *Middleware) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	if err := m.matchAndRecord(OpPut, key); err != nil {
		return storage.PutResult{}, err
	}
	return m.inner.Put(ctx, key, r, opts)
}

// Get consults the rule list. A matching rule returns (nil, err).
func (m *Middleware) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := m.matchAndRecord(OpGet, key); err != nil {
		return nil, err
	}
	return m.inner.Get(ctx, key)
}

// Stat consults the rule list.
func (m *Middleware) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if err := m.matchAndRecord(OpStat, key); err != nil {
		return storage.ObjectInfo{}, err
	}
	return m.inner.Stat(ctx, key)
}

// List consults the rule list using the prefix as the "key" for
// matching. A matching rule returns an iterator that yields a
// single (zero, err) tuple — the same shape List uses for fatal
// errors.
func (m *Middleware) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	if err := m.matchAndRecord(OpList, prefix); err != nil {
		return func(yield func(storage.ObjectInfo, error) bool) {
			yield(storage.ObjectInfo{}, err)
		}
	}
	return m.inner.List(ctx, prefix)
}

// Delete consults the rule list.
func (m *Middleware) Delete(ctx context.Context, key string) error {
	if err := m.matchAndRecord(OpDelete, key); err != nil {
		return err
	}
	return m.inner.Delete(ctx, key)
}

// RenameIfNotExists consults the rule list using the destination
// key for matching (the resilience-relevant key is the one being
// committed).
func (m *Middleware) RenameIfNotExists(ctx context.Context, src, dst string) error {
	if err := m.matchAndRecord(OpRename, dst); err != nil {
		return err
	}
	return m.inner.RenameIfNotExists(ctx, src, dst)
}

// SetRetention consults the rule list.
func (m *Middleware) SetRetention(ctx context.Context, key string, until time.Time, mode storage.WORMMode) error {
	if err := m.matchAndRecord(OpSetRetention, key); err != nil {
		return err
	}
	return m.inner.SetRetention(ctx, key, until, mode)
}

// Capabilities delegates. The wrapper doesn't change which
// optional features the backend supports.
func (m *Middleware) Capabilities() storage.Capabilities {
	return m.inner.Capabilities()
}

// Barrier forwards to the wrapped plugin. Barrier is not yet a
// fault-injectable Op; the durability-mode crash tests inject at
// the Put layer instead.
func (m *Middleware) Barrier(ctx context.Context) error {
	return m.inner.Barrier(ctx)
}

// Close delegates.
func (m *Middleware) Close() error { return m.inner.Close() }

// Region delegates via storage.RegionOf so the residency-check
// helper sees through the wrapper.
func (m *Middleware) Region() string {
	return storage.RegionOf(m.inner)
}

// Compile-time assertion that we satisfy StoragePlugin.
var _ storage.StoragePlugin = (*Middleware)(nil)

// ErrInjected is the canonical sentinel an operator can use as a
// rule's Err when they want a recognisable "this came from fault
// injection" tag (rather than mimicking a backend-specific error).
// Tests and the gameday s3_throttle scenario default to this.
var ErrInjected = errors.New("faultinject: injected fault")
