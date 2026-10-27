package schedule_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/schedule"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/leakcheck"
)

// The scheduler loop on the real clock, firing many thousands of times
// (plus failing and panicking tasks, and a persisted last-run store): heap,
// goroutines and fds must be flat. Each run arms a run-ceiling timer that is
// never stopped; this proves those do not accumulate.
func TestLeak_EngineSteadyState(t *testing.T) {
	want := int64(1200)
	if os.Getenv("PGHS_SCALE") == "1" {
		want = 8000
	}
	store, err := schedule.OpenFileLastRunStore(filepath.Join(t.TempDir(), "lastrun.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int64
	e := schedule.New(schedule.WithLastRunStore(store), schedule.WithJitter(time.Millisecond))
	for _, nm := range []string{"ok", "fail", "panic"} {
		nm := nm
		if err := e.Add(&schedule.Task{
			Name: nm, Schedule: schedule.Every{Interval: time.Millisecond},
			Run: func(ctx context.Context) error {
				runs.Add(1)
				switch nm {
				case "fail":
					return errors.New("boom")
				case "panic":
					panic("boom")
				}
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	base := leakcheck.Take()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	var samples []uint64
	for runs.Load() < want {
		time.Sleep(50 * time.Millisecond)
		if len(samples) == 0 || runs.Load() >= want/8*int64(len(samples)+1) {
			samples = append(samples, leakcheck.LiveHeap())
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("%d runs; heap samples KiB: %v", runs.Load(), func() []uint64 {
		o := make([]uint64, len(samples))
		for i, s := range samples {
			o[i] = s >> 10
		}
		return o
	}())
	if len(samples) >= 4 && samples[len(samples)-1] > samples[1]+8<<20 {
		t.Errorf("engine heap grows with run count: %v", samples)
	}
	_ = e.Tasks()
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{})
}

// A task that ignores cancellation is abandoned once; it must NOT be
// re-launched on top of its still-running predecessor every cadence tick
// (one leaked goroutine per tick, and overlapping instances of a backup).
func TestEngine_AbandonedRunIsNotRelaunchedWhileStillRunning(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)}
	rec := newFinishRecorder()
	e := schedule.New(schedule.WithClock(clock), schedule.WithOnFinish(rec.record))

	var started atomic.Int32
	hang := make(chan struct{})
	if err := e.Add(&schedule.Task{
		Name: "drill:stuck", Schedule: schedule.Every{Interval: time.Hour}, Timeout: time.Minute,
		Run: func(ctx context.Context) error { started.Add(1); <-hang; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	engDone := make(chan struct{})
	go func() { _ = e.Run(ctx); close(engDone) }()
	driveUntil(t, clock, func() bool { return rec.count("drill:stuck") >= 4 }, 60*time.Second)
	if got := started.Load(); got != 1 {
		t.Errorf("wedged task launched %d times; want exactly 1 while the first is still stuck", got)
	}
	// Once it finally returns, the task runs again normally.
	close(hang)
	n := rec.count("drill:stuck")
	driveUntil(t, clock, func() bool { return rec.count("drill:stuck") >= n+2 }, 30*time.Second)
	if got := started.Load(); got < 2 {
		t.Errorf("task never re-ran after the stuck run returned (started=%d)", got)
	}
	cancel()
	<-engDone
}
