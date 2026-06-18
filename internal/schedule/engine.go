// engine.go — Task scheduling engine: cron-style timer + jitter + per-Task Run dispatch.
package schedule

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// Task is one scheduled unit of work.
type Task struct {
	// Name is the human-readable identifier (e.g. "backup:db1",
	// "rotate:db1"). Goes into events the engine emits.
	Name string

	// Schedule predicts when the task next fires.
	Schedule Schedule

	// Run is what the task does. Errors are reported but do not
	// stop the engine. Run must respect ctx.
	Run func(ctx context.Context) error
}

// Engine is the scheduler runtime. It walks Tasks, sleeps until the
// soonest is due, runs it, and repeats until ctx is cancelled.
//
// Concurrency model: tasks fire SERIALLY within an Engine. This is
// the safe default for backup + rotate against the same repo prefix.
// Operators wanting parallel deployments instantiate one Engine per
// deployment.
type Engine struct {
	mu       sync.Mutex
	tasks    []*scheduledTask
	clock    Clock
	onStart  func(name string, due time.Time)
	onFinish func(name string, due time.Time, dur time.Duration, err error)

	// jitter, if positive, spreads simultaneous task firings by up to
	// `jitter` to avoid thundering herds. The first firing of every
	// task gets an offset uniformly drawn from [0, jitter); subsequent
	// firings retain the cadence the Schedule prescribes (so a "every
	// 6h" task with 30s jitter fires at T+x, T+x+6h, T+x+12h, …).
	jitter time.Duration
	rng    *rand.Rand
}

type scheduledTask struct {
	t       *Task
	nextDue time.Time
	lastRun time.Time
	lastErr error
}

// Clock abstracts time.Now / time.After for tests. Production code
// uses RealClock; tests inject a controllable variant.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// RealClock is the production clock.
type RealClock struct{}

// Now implements Clock.
func (RealClock) Now() time.Time { return time.Now() }

// After implements Clock.
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// EngineOption configures an Engine at construction.
type EngineOption func(*Engine)

// WithClock replaces the engine's clock. Tests use this to drive
// time deterministically.
func WithClock(c Clock) EngineOption {
	return func(e *Engine) { e.clock = c }
}

// WithOnStart registers a callback fired just before a task runs.
// Useful for emitting "task.started" events from the agent.
func WithOnStart(fn func(name string, due time.Time)) EngineOption {
	return func(e *Engine) { e.onStart = fn }
}

// WithOnFinish registers a callback fired immediately after a task
// returns. The err argument is the task's return value (nil on
// success). Used for "task.finished" events with duration.
func WithOnFinish(fn func(name string, due time.Time, dur time.Duration, err error)) EngineOption {
	return func(e *Engine) { e.onFinish = fn }
}

// WithJitter spreads first-firing times by up to d. Two tasks with
// identical schedules no longer fire back-to-back-to-back at every
// boundary; they pick up independent random offsets at engine
// construction. Pass 0 (default) to disable.
//
// Implementation detail: jitter is added once, on the FIRST firing
// computed by Add. Subsequent Next() calls flow through the
// schedule unchanged, so the cadence the operator declared is
// preserved exactly past the initial offset.
func WithJitter(d time.Duration) EngineOption {
	return func(e *Engine) {
		e.jitter = d
	}
}

// New returns an empty Engine.
func New(opts ...EngineOption) *Engine {
	e := &Engine{clock: RealClock{}}
	for _, opt := range opts {
		opt(e)
	}
	if e.jitter > 0 {
		// Per-engine RNG seeded with a non-zero source so jitter
		// is non-deterministic across processes; tests using
		// WithJitter usually pin the seed via WithJitterSeed (an
		// optional helper in tests; not exposed publicly to keep
		// the surface minimal).
		e.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return e
}

// Add registers a task. Schedules its first firing relative to the
// engine's current clock. Returns ErrEmptySchedule when the task's
// next-due time is the zero value (meaning the schedule is already
// exhausted, e.g. a "once at" instant in the past).
func (e *Engine) Add(t *Task) error {
	if t == nil {
		return errors.New("schedule: nil task")
	}
	if t.Name == "" {
		return errors.New("schedule: task has empty Name")
	}
	if t.Schedule == nil {
		return errors.New("schedule: task has nil Schedule")
	}
	if t.Run == nil {
		return errors.New("schedule: task has nil Run")
	}
	now := e.clock.Now()
	next := t.Schedule.Next(now)
	if next.IsZero() {
		return ErrEmptySchedule
	}
	if e.jitter > 0 && e.rng != nil {
		// Add a uniformly-random offset in [0, jitter) to the first
		// firing so co-scheduled tasks don't pile on one boundary.
		offset := time.Duration(e.rng.Int63n(int64(e.jitter)))
		next = next.Add(offset)
	}
	e.mu.Lock()
	e.tasks = append(e.tasks, &scheduledTask{t: t, nextDue: next})
	e.mu.Unlock()
	return nil
}

// Tasks returns a snapshot of registered tasks for status output.
func (e *Engine) Tasks() []TaskStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]TaskStatus, 0, len(e.tasks))
	for _, st := range e.tasks {
		ts := TaskStatus{
			Name:        st.t.Name,
			Description: st.t.Schedule.Description(),
			NextDue:     st.nextDue,
			LastRun:     st.lastRun,
		}
		if st.lastErr != nil {
			ts.LastError = st.lastErr.Error()
		}
		out = append(out, ts)
	}
	return out
}

// TaskStatus is a serialisable view of a task for the agent's status
// output. Stable shape per the v1 schema commitment.
type TaskStatus struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	NextDue     time.Time `json:"next_due"`
	LastRun     time.Time `json:"last_run,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

// Run executes scheduled tasks until ctx is cancelled. Returns ctx.Err()
// on cancellation. If the engine has zero tasks, Run returns
// ErrNoTasks immediately — the caller decides whether that's an
// error (an agent with no work to do is suspicious).
func (e *Engine) Run(ctx context.Context) error {
	e.mu.Lock()
	if len(e.tasks) == 0 {
		e.mu.Unlock()
		return ErrNoTasks
	}
	e.mu.Unlock()

	for {
		// Top-of-loop ctx check. pickNext() takes the engine's mutex
		// and walks the task list; if we entered the iteration with
		// an already-cancelled ctx, the post-pickNext check (after
		// wait) was the only point that bailed — meaning one extra
		// task would run before exit. The check here is microseconds
		// and makes "cancel-and-shut-down" return immediately when
		// no task is currently due.
		if err := ctx.Err(); err != nil {
			return err
		}
		next, dueAt := e.pickNext()
		if next == nil {
			// All schedules exhausted (every Once has fired). Clean exit.
			return nil
		}
		now := e.clock.Now()
		wait := dueAt.Sub(now)
		if wait > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-e.clock.After(wait):
			}
		} else {
			// Already due — yield briefly so we don't starve
			// ctx-cancellation when many tasks are simultaneously due.
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}

		// Re-check ctx; the wait may have just expired but ctx might
		// have been cancelled mid-wait.
		if err := ctx.Err(); err != nil {
			return err
		}

		e.runOne(ctx, next, dueAt)
	}
}

// pickNext returns the soonest-due task and the time at which it's due.
// Removes one-shot tasks whose schedule has been exhausted.
func (e *Engine) pickNext() (*scheduledTask, time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Prune any scheduledTask whose nextDue is zero (one-shots that
	// already fired in a previous tick). We do this before sorting so
	// the live list is the one we sort.
	live := e.tasks[:0]
	for _, st := range e.tasks {
		if !st.nextDue.IsZero() {
			live = append(live, st)
		}
	}
	e.tasks = live

	if len(e.tasks) == 0 {
		return nil, time.Time{}
	}

	sort.SliceStable(e.tasks, func(i, j int) bool {
		return e.tasks[i].nextDue.Before(e.tasks[j].nextDue)
	})
	return e.tasks[0], e.tasks[0].nextDue
}

// runOne executes the task and records the outcome. Recovers panics
// — a panicking task must not crash the agent.
func (e *Engine) runOne(ctx context.Context, st *scheduledTask, dueAt time.Time) {
	if e.onStart != nil {
		e.onStart(st.t.Name, dueAt)
	}
	start := e.clock.Now()
	err := safeRun(ctx, st.t.Run)
	dur := e.clock.Now().Sub(start)
	if e.onFinish != nil {
		e.onFinish(st.t.Name, dueAt, dur, err)
	}

	// Anchor the next firing to the SCHEDULED time (dueAt), NOT the
	// completion time. Computing Next(now) pushes every subsequent firing
	// later by however long the run took, so an `every 6h` task that takes
	// 30m fires every 6h30m — cumulative drift that silently under-runs the
	// operator's configured cadence/RPO. (DailyAt is unaffected either way —
	// Next always returns the next HH:MM — but Every drifts.)
	//
	// Catch-up: if the run overran one or more whole intervals (so the next
	// grid slot is already in the past), skip forward to the first future
	// slot rather than firing back-to-back to "make up" missed slots. The
	// !advance.After(next) guard stops a degenerate non-advancing schedule
	// from looping; Once returns zero on the first Next and exits the loop.
	next := st.t.Schedule.Next(dueAt)
	now := e.clock.Now()
	for !next.IsZero() && !next.After(now) {
		adv := st.t.Schedule.Next(next)
		if adv.IsZero() || !adv.After(next) {
			break
		}
		next = adv
	}

	e.mu.Lock()
	st.lastRun = start
	st.lastErr = err
	st.nextDue = next
	e.mu.Unlock()
}

// safeRun runs fn under a panic-recovery shim so the engine survives
// a buggy task.
func safeRun(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("schedule: task panicked: %v", r)
		}
	}()
	return fn(ctx)
}

// ErrNoTasks is returned by Engine.Run when no tasks are registered.
var ErrNoTasks = errors.New("schedule: engine has no tasks")

// ErrEmptySchedule is returned by Engine.Add when the task's first
// firing is already in the past with no future firings (a Once
// pointing backwards). The task is rejected — easier to surface this
// at config time than to silently never run.
var ErrEmptySchedule = errors.New("schedule: task has no future firings")
