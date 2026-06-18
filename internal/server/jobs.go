// jobs.go — dispatch types and in-memory JobRegistry: enqueue,
// claim, progress, completion, plus the JobKind/JobState enums.

package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// JobState is the dispatch lifecycle.
type JobState string

const (
	// JobQueued is the initial state — created by an operator's POST,
	// not yet claimed by any agent.
	JobQueued JobState = "queued"

	// JobRunning is set when an agent claims a job. Progress events
	// extend the Progress slice; the running state persists until
	// the agent posts /complete or the dispatch deadline fires.
	JobRunning JobState = "running"

	// JobCompleted is the terminal success state.
	JobCompleted JobState = "completed"

	// JobFailed is the terminal failure state. Failure carries the
	// agent-supplied or dispatcher-derived reason.
	JobFailed JobState = "failed"

	// JobCancelled is operator-initiated termination.
	JobCancelled JobState = "cancelled"
)

// JobKind enumerates the dispatchable operations. v0.4 ships
// `backup` end-to-end; `restore` and `verify` are accepted by the
// dispatcher but the agent-side runner integration is+.
type JobKind string

const (
	// JobBackup runs a BASE_BACKUP capture against the target
	// deployment. End-to-end wired in v0.4.
	JobBackup JobKind = "backup"

	// JobRestore materialises a backup into a target datadir.
	// Dispatcher-accepted; agent-side runner integration pending.
	JobRestore JobKind = "restore"

	// JobVerify runs the verify pipeline (manifest parse + chunk
	// reachability + signature) against a committed backup.
	// Dispatcher-accepted; agent-side runner integration pending.
	JobVerify JobKind = "verify"
)

// Job is one dispatchable unit of work. Field set is small
// deliberately: the control plane is the orchestrator, not the
// system of record. The agent does the heavy lifting and reports
// back; the Job only carries enough state to coordinate.
type Job struct {
	ID         string         `json:"id"`
	Kind       JobKind        `json:"kind"`
	Deployment string         `json:"deployment"`
	RepoURL    string         `json:"repo_url"`
	Args       map[string]any `json:"args,omitempty"`

	State       JobState   `json:"state"`
	AssignedTo  string     `json:"assigned_to,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	Progress []ProgressEvent `json:"progress,omitempty"`
	// ProgressDropped counts progress events discarded to keep Progress
	// bounded (memory-leak audit #3). Non-zero means Progress holds only
	// the most recent maxProgressEvents observations, not the full
	// history — surfaced so an observer can tell the tail was truncated.
	ProgressDropped int64          `json:"progress_dropped,omitempty"`
	Result          map[string]any `json:"result,omitempty"`
	Failure         string         `json:"failure,omitempty"`
}

// maxProgressEvents caps how many progress events the in-memory backend
// retains per job. A long-running backup can emit thousands; without a
// cap a single job's Progress slice grows unbounded for its whole
// lifetime, and cloneJob copies the entire slice on every Get/List
// (memory-leak audit #3). When the cap is exceeded the OLDEST events are
// dropped (the recent tail is what a status poll wants) and
// ProgressDropped records how many were shed.
const maxProgressEvents = 1000

// ProgressEvent is one observation emitted by the agent during
// execution. The shape mirrors the agent's NDJSON event stream so a
// future direct-stream path can replace the polling protocol without
// reshaping the Job.
type ProgressEvent struct {
	At   time.Time      `json:"at"`
	Op   string         `json:"op,omitempty"`
	Body map[string]any `json:"body,omitempty"`
}

// EnqueueOptions tunes Enqueue.
type EnqueueOptions struct {
	Kind       JobKind
	Deployment string
	RepoURL    string
	Args       map[string]any
}

// ListOptions filter List output.
type ListOptions struct {
	State      JobState
	Kind       JobKind
	Deployment string
	Limit      int
}

// ClaimOptions tunes Claim.
type ClaimOptions struct {
	AgentID     string
	Deployments []string
	Kinds       []JobKind

	// MaxConcurrent caps how many jobs may be in JobRunning state at
	// once. A claim is refused with ErrNoJobs once the running count
	// reaches it, so queued work stays queued and agents keep polling
	// until a slot frees — backpressure that stops a burst of claims
	// from storming storage / PostgreSQL with unbounded concurrent
	// backups. Zero (or negative) means unlimited. Set by the
	// JobRegistry from its WithMaxConcurrent setting; the backends
	// enforce it (the memory backend as a hard cap under its lock, the
	// PG backend globally in SQL across control planes).
	MaxConcurrent int
}

// CompleteOptions tunes Complete.
type CompleteOptions struct {
	Success bool
	Result  map[string]any
	Failure string
}

// JobRegistry is the dispatch facade. It wraps a JobBackend (memory
// or PG) and adds the cross-backend behaviours: claim deadline,
// background sweeper, default constructor.
//
// Existing callers of NewJobRegistry() continue to get an in-memory
// backend; callers that want persistence pass a PGBackend via
// NewJobRegistryWithBackend.
// terminalPruner is the OPTIONAL capability a backend implements to
// bound retained terminal jobs. The in-memory backend implements it (its
// map would otherwise grow forever — memory-leak audit #2); the PG
// backend is durable + queryable and can ship its own retention later,
// so it's not forced onto the JobBackend interface.
type terminalPruner interface {
	PruneTerminal(ctx context.Context, olderThan time.Duration) (int, error)
}

// defaultTerminalRetention is how long a finished job stays queryable
// before the sweeper prunes it from a pruning backend.
const defaultTerminalRetention = 24 * time.Hour

type JobRegistry struct {
	backend           JobBackend
	mu                sync.RWMutex
	claimDeadline     time.Duration
	maxConcurrent     int
	terminalRetention time.Duration

	// sweeperWG tracks the background sweeper goroutine so
	// callers can shut down cleanly. Without this the goroutine
	// outlives the registry's caller and leaks past test
	// boundaries (`-race` flags it). RunSweeper registers; Stop
	// waits for the registered goroutine to drain.
	sweeperWG sync.WaitGroup

	// sweeperCancels holds one cancel func per RunSweeper goroutine,
	// each cancelling a context DERIVED from the caller's ctx. Stop
	// fires them before waiting, so Stop can't hang even if the caller
	// never cancelled the ctx it passed to RunSweeper (deadlock audit
	// #1). Guarded by mu.
	sweeperCancels []context.CancelFunc
}

// NewJobRegistry returns a registry backed by a fresh in-memory
// backend. Equivalent to NewJobRegistryWithBackend(NewMemoryBackend()).
// Default claim deadline is 6h (generous for big-DB backups).
func NewJobRegistry() *JobRegistry {
	return NewJobRegistryWithBackend(NewMemoryBackend())
}

// NewJobRegistryWithBackend wraps a custom backend. The PG backend
// flows through this constructor.
func NewJobRegistryWithBackend(b JobBackend) *JobRegistry {
	return &JobRegistry{
		backend:           b,
		claimDeadline:     6 * time.Hour,
		terminalRetention: defaultTerminalRetention,
	}
}

// WithTerminalRetention overrides how long finished jobs are retained
// before the sweeper prunes them (pruning backends only). A non-positive
// value disables pruning — finished jobs are kept forever (the pre-fix
// behaviour; only safe for a short-lived process).
func (r *JobRegistry) WithTerminalRetention(d time.Duration) *JobRegistry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.terminalRetention = d
	return r
}

// PruneTerminal asks the backend to drop terminal jobs older than the
// configured retention. No-op when the backend doesn't support pruning
// or retention is disabled. Returns the number pruned.
func (r *JobRegistry) PruneTerminal() int {
	p, ok := r.backend.(terminalPruner)
	if !ok {
		return 0
	}
	r.mu.RLock()
	retention := r.terminalRetention
	r.mu.RUnlock()
	if retention <= 0 {
		return 0
	}
	n, _ := p.PruneTerminal(context.Background(), retention)
	return n
}

// Backend returns the wrapped backend. Used by Server.Close to
// release backend resources at shutdown; tests pull it for direct
// access when bypassing the facade is cleaner than going through it.
func (r *JobRegistry) Backend() JobBackend { return r.backend }

// WithClaimDeadline overrides the default. Useful for tests that
// want fast reclamation, and for fleets with much longer expected
// backup durations.
func (r *JobRegistry) WithClaimDeadline(d time.Duration) *JobRegistry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d > 0 {
		r.claimDeadline = d
	}
	return r
}

// ClaimDeadline returns the current deadline.
func (r *JobRegistry) ClaimDeadline() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.claimDeadline
}

// WithMaxConcurrent caps how many jobs may run at once across the
// fleet (see ClaimOptions.MaxConcurrent). Zero or negative disables
// the cap (unlimited — the default). For multi-control-plane HA, set
// the same value on every control plane: the PG backend enforces it
// globally via the shared jobs table.
func (r *JobRegistry) WithMaxConcurrent(n int) *JobRegistry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n < 0 {
		n = 0
	}
	r.maxConcurrent = n
	return r
}

// MaxConcurrent returns the configured cap (0 = unlimited).
func (r *JobRegistry) MaxConcurrent() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.maxConcurrent
}

// --- delegating methods (with ctx-less compat shims) -----------------
//
// The historic surface didn't take context.Context; the existing
// callers in routes.go pass r.Context() through Enqueue / Claim
// indirectly. We accept the slight redundancy of two method shapes
// (Enqueue + EnqueueCtx) for the transition; once every caller
// passes ctx explicitly the non-ctx shapes can retire. most
// likely.

// Enqueue records a new job. Uses context.Background() — see method
// note.
func (r *JobRegistry) Enqueue(opts EnqueueOptions) (*Job, error) {
	return r.backend.Enqueue(context.Background(), opts)
}

// Get returns a job snapshot.
func (r *JobRegistry) Get(id string) (*Job, error) {
	return r.backend.Get(context.Background(), id)
}

// List returns jobs matching opts.
func (r *JobRegistry) List(opts ListOptions) []Job {
	out, err := r.backend.List(context.Background(), opts)
	if err != nil {
		return nil
	}
	return out
}

// Claim atomically transitions the oldest queued job matching opts
// to JobRunning, subject to the registry's concurrency cap.
func (r *JobRegistry) Claim(opts ClaimOptions) (*Job, error) {
	opts.MaxConcurrent = r.MaxConcurrent()
	return r.backend.Claim(context.Background(), opts)
}

// AppendProgress records one progress event on a running job.
func (r *JobRegistry) AppendProgress(id string, ev ProgressEvent) error {
	return r.backend.AppendProgress(context.Background(), id, ev)
}

// Complete transitions the job to a terminal state. Idempotent on
// already-terminal jobs.
func (r *JobRegistry) Complete(id string, opts CompleteOptions) (*Job, error) {
	return r.backend.Complete(context.Background(), id, opts)
}

// Cancel transitions a queued or running job to JobCancelled.
func (r *JobRegistry) Cancel(id, reason string) (*Job, error) {
	return r.backend.Cancel(context.Background(), id, reason)
}

// SweepAbandoned reclaims every Running job whose UpdatedAt (last agent
// activity) is older than the claim deadline.
func (r *JobRegistry) SweepAbandoned() int {
	r.mu.RLock()
	deadline := r.claimDeadline
	r.mu.RUnlock()
	n, _ := r.backend.SweepAbandoned(context.Background(), deadline)
	return n
}

// RunSweeper starts a goroutine that calls SweepAbandoned on the
// supplied interval until ctx cancels.
//
// The goroutine is tracked via the registry's sweeperWG so callers
// can wait for orderly shutdown via Stop(). RunSweeper may be
// called more than once (e.g. tests that re-arm the sweeper after
// a sub-test) — each call adds one tracked goroutine; ctx
// cancellation (or Stop) drains all of them.
// onTick, when non-nil, is invoked once per sweeper tick with the number
// of jobs reclaimed and any error from the pass. A failing or panicking
// backend surfaces here instead of being silently dropped (poor-error-
// handling audit #4) — the supplied callback decides how to log it.
func (r *JobRegistry) RunSweeper(ctx context.Context, interval time.Duration, onTick func(reaped int, err error)) {
	if interval <= 0 {
		interval = time.Minute
	}
	// Derive a cancellable context from the caller's so Stop can stop
	// this goroutine itself, without depending on the caller cancelling
	// ctx first (deadlock audit #1). Cancelling the caller's ctx still
	// works — it propagates to sweepCtx.
	sweepCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.sweeperCancels = append(r.sweeperCancels, cancel)
	r.mu.Unlock()
	r.sweeperWG.Add(1)
	go func() {
		defer r.sweeperWG.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-t.C:
				reaped, err := r.sweepTick(sweepCtx)
				if onTick != nil {
					onTick(reaped, err)
				}
			}
		}
	}()
}

// sweepTick runs one reclaim + terminal-prune pass. It recovers any panic
// from a misbehaving backend into an error — without this, a panic in the
// long-lived sweeper goroutine would crash the entire control plane
// (poor-error-handling audit #4) — and returns the first backend error so
// the caller can surface it rather than dropping it. Terminal-job pruning
// (memory-leak audit #2) still runs each tick.
func (r *JobRegistry) sweepTick(ctx context.Context) (reaped int, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("jobs: sweeper panicked: %v", rec)
		}
	}()
	r.mu.RLock()
	deadline := r.claimDeadline
	retention := r.terminalRetention
	r.mu.RUnlock()

	reaped, err = r.backend.SweepAbandoned(ctx, deadline)
	if p, ok := r.backend.(terminalPruner); ok && retention > 0 {
		if _, perr := p.PruneTerminal(ctx, retention); perr != nil && err == nil {
			err = perr
		}
	}
	return reaped, err
}

// Stop cancels every sweeper goroutine started via RunSweeper and
// blocks until they exit. It is self-sufficient: it cancels the
// (derived) sweeper contexts itself, so it returns promptly whether or
// not the caller already cancelled the ctx it passed to RunSweeper
// (deadlock audit #1). Idempotent, and a no-op when no sweeper was
// started — Wait on a zero-counter WaitGroup returns immediately.
func (r *JobRegistry) Stop() {
	r.mu.Lock()
	cancels := r.sweeperCancels
	r.sweeperCancels = nil
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	r.sweeperWG.Wait()
}

// --- helpers ---------------------------------------------------------

// newJobID generates a 16-byte hex ID. Same shape across backends so
// IDs are interchangeable; only the storage layer differs.
//
// crypto/rand failure on a working OS is essentially never
// observed; if it does happen, returning zero bytes would collide
// every job ID and break Enqueue's uniqueness invariant (two
// concurrent enqueues would write to the same backend key).
// Panic + supervisor restart is the documented recovery path —
// same posture as audit.realRandomBytes.
func newJobID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("jobs: rand.Read: %v", err))
	}
	return hex.EncodeToString(b[:])
}
