package schedule_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/schedule"
)

// The engine fires tasks serially with NO deadline of any kind: one
// wedged Run (hung docker daemon during a drill, D-state I/O during a
// backup) used to stop every other scheduled task on the agent
// forever, while the process stayed alive and looked healthy. These
// tests pin the run-ceiling behaviour: a task that overruns its
// ceiling is cancelled (and, if it ignores the cancel, abandoned) and
// the OTHER tasks keep firing.

// finishRecorder collects onFinish callbacks thread-safely.
type finishRecorder struct {
	mu   sync.Mutex
	errs map[string][]error
}

func newFinishRecorder() *finishRecorder {
	return &finishRecorder{errs: make(map[string][]error)}
}

func (r *finishRecorder) record(name string, _ time.Time, _ time.Duration, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs[name] = append(r.errs[name], err)
}

func (r *finishRecorder) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.errs[name])
}

func (r *finishRecorder) firstErr(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.errs[name]) == 0 {
		return nil
	}
	return r.errs[name][0]
}

// driveUntil advances the fake clock in a background loop until cond
// holds or the real-time deadline passes. Over-advancing is safe: the
// fake clock's After channels are buffered and fire on every advance.
func driveUntil(t *testing.T, clock *fakeClock, cond func() bool, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		clock.advance(time.Hour)
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not reached within %s", within)
}

// A well-behaved but never-returning task (blocks on ctx) must be
// cancelled at its run ceiling so later tasks fire.
func TestEngine_WedgedTaskDoesNotStarveOthers(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)}
	rec := newFinishRecorder()
	e := schedule.New(schedule.WithClock(clock), schedule.WithOnFinish(rec.record))

	wedged, _ := schedule.Parse(schedule.Spec{Every: "1h"})
	if err := e.Add(&schedule.Task{
		Name:     "drill:wedged",
		Schedule: wedged,
		Run: func(ctx context.Context) error {
			<-ctx.Done() // honors cancel, but would otherwise block forever
			return ctx.Err()
		},
	}); err != nil {
		t.Fatal(err)
	}
	healthy, _ := schedule.Parse(schedule.Spec{Every: "2h"})
	if err := e.Add(&schedule.Task{
		Name:     "backup:db1",
		Schedule: healthy,
		Run:      func(ctx context.Context) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engDone := make(chan struct{})
	go func() { _ = e.Run(ctx); close(engDone) }()

	driveUntil(t, clock, func() bool { return rec.count("backup:db1") >= 1 }, 15*time.Second)

	if got := rec.firstErr("drill:wedged"); got == nil {
		t.Error("wedged task finished nil — expected a cancellation/ceiling error")
	}
	cancel()
	<-engDone
}

// A task that IGNORES cancellation entirely must be abandoned after
// the grace period — the engine's serial loop moves on regardless.
func TestEngine_CancelIgnoringTaskIsAbandoned(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)}
	rec := newFinishRecorder()
	e := schedule.New(schedule.WithClock(clock), schedule.WithOnFinish(rec.record))

	hang := make(chan struct{}) // never closed
	wedged, _ := schedule.Parse(schedule.Spec{Every: "1h"})
	if err := e.Add(&schedule.Task{
		Name:     "drill:stuck",
		Schedule: wedged,
		Timeout:  time.Minute,
		Run: func(ctx context.Context) error {
			<-hang // D-state simulator: ctx is ignored
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	healthy, _ := schedule.Parse(schedule.Spec{Every: "2h"})
	if err := e.Add(&schedule.Task{
		Name:     "backup:db1",
		Schedule: healthy,
		Run:      func(ctx context.Context) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engDone := make(chan struct{})
	go func() { _ = e.Run(ctx); close(engDone) }()

	// The abandonment path includes a 5 s real-time grace, so allow
	// generous real time here.
	driveUntil(t, clock, func() bool { return rec.count("backup:db1") >= 1 }, 30*time.Second)

	err := rec.firstErr("drill:stuck")
	if err == nil || !strings.Contains(err.Error(), "abandoned") {
		t.Errorf("stuck task error = %v, want the run-ceiling abandonment error", err)
	}
	cancel()
	<-engDone
	close(hang) // release the leaked goroutine for -race hygiene
}
