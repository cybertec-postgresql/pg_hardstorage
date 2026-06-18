// jobs_memory.go — MemoryBackend: single-instance in-memory JobBackend (lost on restart).
package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemoryBackend is the in-memory JobBackend. Concurrency: every
// public method takes the appropriate Lock; the map is small enough
// that even busy claim/progress loops don't show contention. Lost
// on process restart — operators who need persistence pick
// PGBackend.
type MemoryBackend struct {
	mu   sync.RWMutex
	jobs map[string]*Job
}

// NewMemoryBackend returns an empty backend. callers wire this
// (or PGBackend) into NewJobRegistryWithBackend; the existing
// NewJobRegistry continues to default to memory.
func NewMemoryBackend() *MemoryBackend {
	return &MemoryBackend{jobs: map[string]*Job{}}
}

// Enqueue implements JobBackend.
func (b *MemoryBackend) Enqueue(_ context.Context, opts EnqueueOptions) (*Job, error) {
	if opts.Kind == "" {
		return nil, errors.New("jobs: Kind is required")
	}
	if opts.Deployment == "" {
		return nil, errors.New("jobs: Deployment is required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now().UTC()
	j := &Job{
		ID:         newJobID(),
		Kind:       opts.Kind,
		Deployment: opts.Deployment,
		RepoURL:    opts.RepoURL,
		Args:       opts.Args,
		State:      JobQueued,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	b.jobs[j.ID] = j
	return cloneJob(j), nil
}

// Get implements JobBackend.
func (b *MemoryBackend) Get(_ context.Context, id string) (*Job, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	j, ok := b.jobs[id]
	if !ok {
		return nil, ErrJobNotFound
	}
	return cloneJob(j), nil
}

// List implements JobBackend.
func (b *MemoryBackend) List(_ context.Context, opts ListOptions) ([]Job, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Job, 0, len(b.jobs))
	for _, j := range b.jobs {
		if opts.State != "" && j.State != opts.State {
			continue
		}
		if opts.Kind != "" && j.Kind != opts.Kind {
			continue
		}
		if opts.Deployment != "" && j.Deployment != opts.Deployment {
			continue
		}
		out = append(out, *cloneJob(j))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

// Claim implements JobBackend.
func (b *MemoryBackend) Claim(_ context.Context, opts ClaimOptions) (*Job, error) {
	if opts.AgentID == "" {
		return nil, errors.New("jobs: AgentID is required for claim")
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	// Concurrency cap: refuse the claim (as ErrNoJobs, so agents simply
	// keep polling) once the fleet is at the running limit. Counted
	// under the same lock as the claim below, so this is a hard cap.
	if opts.MaxConcurrent > 0 {
		running := 0
		for _, j := range b.jobs {
			if j.State == JobRunning {
				running++
			}
		}
		if running >= opts.MaxConcurrent {
			return nil, ErrNoJobs
		}
	}

	deploymentSet := map[string]struct{}{}
	for _, d := range opts.Deployments {
		deploymentSet[d] = struct{}{}
	}
	kindSet := map[JobKind]struct{}{}
	for _, k := range opts.Kinds {
		kindSet[k] = struct{}{}
	}

	type cand struct {
		j  *Job
		at time.Time
	}
	var cands []cand
	for _, j := range b.jobs {
		if j.State != JobQueued {
			continue
		}
		if len(deploymentSet) > 0 {
			if _, ok := deploymentSet[j.Deployment]; !ok {
				continue
			}
		}
		if len(kindSet) > 0 {
			if _, ok := kindSet[j.Kind]; !ok {
				continue
			}
		}
		cands = append(cands, cand{j: j, at: j.CreatedAt})
	}
	if len(cands) == 0 {
		return nil, ErrNoJobs
	}
	sort.Slice(cands, func(i, k int) bool { return cands[i].at.Before(cands[k].at) })
	pick := cands[0].j
	now := time.Now().UTC()
	pick.State = JobRunning
	pick.AssignedTo = opts.AgentID
	pick.UpdatedAt = now
	pick.StartedAt = &now
	return cloneJob(pick), nil
}

// AppendProgress implements JobBackend.
func (b *MemoryBackend) AppendProgress(_ context.Context, id string, ev ProgressEvent) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	j, ok := b.jobs[id]
	if !ok {
		return ErrJobNotFound
	}
	if j.State != JobRunning {
		return ErrJobNotRunning
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	j.Progress = append(j.Progress, ev)
	// Bound the retained history: keep the most recent maxProgressEvents
	// and shed the oldest, recording how many were dropped so the
	// truncation isn't silent (memory-leak audit #3). Shifting keeps the
	// slice header pointing at a fresh backing array via copy so the
	// dropped events become collectable.
	if over := len(j.Progress) - maxProgressEvents; over > 0 {
		kept := make([]ProgressEvent, maxProgressEvents)
		copy(kept, j.Progress[over:])
		j.Progress = kept
		j.ProgressDropped += int64(over)
	}
	j.UpdatedAt = ev.At
	return nil
}

// Complete implements JobBackend.
func (b *MemoryBackend) Complete(_ context.Context, id string, opts CompleteOptions) (*Job, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	j, ok := b.jobs[id]
	if !ok {
		return nil, ErrJobNotFound
	}
	if j.State == JobCompleted || j.State == JobFailed || j.State == JobCancelled {
		// Claim-fence (race-condition audit #3): a SUCCESS report against
		// a Failed or Cancelled job means our claim was terminated out
		// from under us — the sweeper marked it abandoned, or an operator
		// cancelled it — while we were still running. Surface that instead
		// of silently discarding a successful backup's result. A failure
		// report, or a repeat success on an already-Completed job, stays
		// idempotent.
		if opts.Success && j.State != JobCompleted {
			return nil, ErrClaimLost
		}
		return cloneJob(j), nil
	}
	now := time.Now().UTC()
	j.UpdatedAt = now
	j.CompletedAt = &now
	if opts.Success {
		j.State = JobCompleted
		j.Result = opts.Result
	} else {
		j.State = JobFailed
		j.Failure = opts.Failure
		if j.Failure == "" {
			j.Failure = "agent reported failure with no message"
		}
	}
	return cloneJob(j), nil
}

// Cancel implements JobBackend.
func (b *MemoryBackend) Cancel(_ context.Context, id, reason string) (*Job, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	j, ok := b.jobs[id]
	if !ok {
		return nil, ErrJobNotFound
	}
	if j.State == JobCompleted || j.State == JobFailed || j.State == JobCancelled {
		return cloneJob(j), nil
	}
	now := time.Now().UTC()
	j.State = JobCancelled
	j.UpdatedAt = now
	j.CompletedAt = &now
	j.Failure = "cancelled: " + reason
	return cloneJob(j), nil
}

// SweepAbandoned implements JobBackend.
func (b *MemoryBackend) SweepAbandoned(_ context.Context, deadline time.Duration) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now().UTC()
	reaped := 0
	for _, j := range b.jobs {
		if j.State != JobRunning || j.StartedAt == nil {
			continue
		}
		// Liveness keys on UpdatedAt (last AppendProgress / activity), NOT
		// StartedAt: a healthy job that keeps reporting progress must
		// never be reclaimed no matter how long it runs. Keying on
		// StartedAt declared a >deadline backup "abandoned" while it was
		// actively progressing, letting a second agent claim the same job.
		// Mirrors PGBackend.SweepAbandoned.
		if now.Sub(j.UpdatedAt) <= deadline {
			continue
		}
		j.State = JobFailed
		j.UpdatedAt = now
		j.CompletedAt = &now
		j.Failure = fmt.Sprintf("abandoned: agent stopped reporting (claim deadline %s elapsed)", deadline)
		reaped++
	}
	return reaped, nil
}

// PruneTerminal deletes terminal (Completed/Failed/Cancelled) jobs whose
// CompletedAt is older than now-olderThan, returning the count removed.
// Without this the jobs map grows for the life of the process — every
// job ever dispatched stays resident even after it finishes (memory-leak
// audit #2). A non-positive olderThan prunes nothing (retain forever).
// Jobs still queued or running are never pruned regardless of age.
func (b *MemoryBackend) PruneTerminal(_ context.Context, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cutoff := time.Now().UTC().Add(-olderThan)
	removed := 0
	for id, j := range b.jobs {
		switch j.State {
		case JobCompleted, JobFailed, JobCancelled:
		default:
			continue // queued/running — keep
		}
		if j.CompletedAt == nil || j.CompletedAt.After(cutoff) {
			continue // still within the retention window
		}
		delete(b.jobs, id)
		removed++
	}
	return removed, nil
}

// Close implements JobBackend. No-op for in-memory.
func (b *MemoryBackend) Close() error { return nil }

// cloneJob returns a defensive copy. Used at every backend → caller
// boundary so callers can't mutate the backend's internal state.
func cloneJob(j *Job) *Job {
	if j == nil {
		return nil
	}
	out := *j
	if j.Progress != nil {
		out.Progress = append([]ProgressEvent(nil), j.Progress...)
	}
	if j.Args != nil {
		out.Args = make(map[string]any, len(j.Args))
		for k, v := range j.Args {
			out.Args[k] = v
		}
	}
	if j.Result != nil {
		out.Result = make(map[string]any, len(j.Result))
		for k, v := range j.Result {
			out.Result[k] = v
		}
	}
	if j.StartedAt != nil {
		t := *j.StartedAt
		out.StartedAt = &t
	}
	if j.CompletedAt != nil {
		t := *j.CompletedAt
		out.CompletedAt = &t
	}
	return &out
}
