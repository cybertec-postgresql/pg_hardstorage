// jobs_backend.go — JobBackend persistence interface satisfied by MemoryBackend + PGBackend.
package server

import (
	"context"
	"errors"
	"time"
)

// JobBackend is the persistence interface every JobRegistry storage
// implementation satisfies. ships two:
//
//   - MemoryBackend (default): in-memory map. Lost on restart;
//     suitable for single-instance control planes that don't need
//     dispatch state to survive process exit.
//   - PGBackend: PostgreSQL-backed. Schema lives in a `pg_hardstorage`
//     namespace inside any reachable PG. FOR UPDATE SKIP LOCKED on
//     claim gives us atomic multi-instance dispatch without a
//     bespoke distributed-lock subsystem. Persistent across
//     restarts; multi-control-plane HA works out of the box.
//
// Every method takes context.Context so the SQL backend can honour
// cancellation through to the underlying query. The in-memory
// backend ignores ctx (its operations are essentially instantaneous).
//
// Why an interface and not a sum type: backend choice is a deploy-
// time configuration, the operator picks one, and the rest of the
// agent doesn't care which is loaded. The interface lets us
// introduce an etcd backend or a Redis backend (third-party)
// without reshaping the call sites.
type JobBackend interface {
	// Enqueue records a new job in JobQueued state. Implementations
	// generate the Job.ID; the rest of the fields come from opts.
	// Returned Job is a snapshot; the backend keeps its own copy.
	Enqueue(ctx context.Context, opts EnqueueOptions) (*Job, error)

	// Get returns a snapshot of the named job. Returns ErrJobNotFound
	// when the ID isn't recorded.
	Get(ctx context.Context, id string) (*Job, error)

	// List returns jobs matching opts, sorted newest-first. Limit ≤ 0
	// means no cap.
	List(ctx context.Context, opts ListOptions) ([]Job, error)

	// Claim atomically transitions the oldest queued job matching
	// opts to JobRunning under opts.AgentID. Returns ErrNoJobs when
	// nothing's eligible. The atomic guarantee is the whole point
	// of the backend abstraction — the in-memory backend uses a
	// mutex; the PG backend uses FOR UPDATE SKIP LOCKED.
	Claim(ctx context.Context, opts ClaimOptions) (*Job, error)

	// AppendProgress records one progress event on a running job.
	// Returns ErrJobNotFound or ErrJobNotRunning when the state
	// doesn't permit progress reporting.
	AppendProgress(ctx context.Context, id string, ev ProgressEvent) error

	// Complete transitions the job to a terminal state. Idempotent
	// on already-terminal jobs (re-completion is a no-op rather
	// than an error) so an agent retrying after a network blip
	// doesn't trip the state machine.
	Complete(ctx context.Context, id string, opts CompleteOptions) (*Job, error)

	// Cancel transitions a queued or running job to JobCancelled.
	// Best-effort for running jobs (the agent observes the cancel
	// only on its next control-plane round-trip).
	Cancel(ctx context.Context, id, reason string) (*Job, error)

	// SweepAbandoned reclaims every Running job whose UpdatedAt (last
	// agent activity — AppendProgress bumps it) is older than now-deadline,
	// transitioning them to JobFailed with a structured "abandoned"
	// message. Keying on UpdatedAt rather than StartedAt means a job that
	// keeps reporting progress is never reclaimed however long it runs;
	// only an agent that genuinely stopped reporting loses its claim.
	// Returns the number reaped.
	SweepAbandoned(ctx context.Context, deadline time.Duration) (int, error)

	// Close releases backend resources. Memory backend is a no-op;
	// PG backend closes the pgx pool.
	Close() error
}

// Errors returned by JobBackend implementations. Defined here (not
// in the per-backend file) so callers can errors.Is against a single
// canonical sentinel regardless of which backend produced them.
var (
	ErrJobNotFound   = errors.New("jobs: not found")
	ErrJobNotRunning = errors.New("jobs: not in running state")
	ErrNoJobs        = errors.New("jobs: no eligible jobs")

	// ErrClaimLost is returned by Complete when an agent reports SUCCESS
	// for a job that is already Failed or Cancelled — i.e. the sweeper
	// reclaimed it as abandoned (or an operator cancelled it) while the
	// agent was still running. Surfacing it instead of silently
	// discarding the result lets the agent learn its claim was lost
	// rather than believe the backup was recorded (race-condition audit
	// #3).
	ErrClaimLost = errors.New("jobs: claim lost — the job was reclaimed (abandoned by the sweeper) or cancelled while running")
)
