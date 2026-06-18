package schedule_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/schedule"
)

func TestParse_Every(t *testing.T) {
	s, err := schedule.Parse(schedule.Spec{Every: "6h"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	got := s.Next(now)
	want := now.Add(6 * time.Hour)
	if !got.Equal(want) {
		t.Errorf("Next(%v) = %v, want %v", now, got, want)
	}
	if !strings.Contains(s.Description(), "every 6h") {
		t.Errorf("Description: %q", s.Description())
	}
}

func TestParse_DailyAt(t *testing.T) {
	s, err := schedule.Parse(schedule.Spec{DailyAt: "04:30"})
	if err != nil {
		t.Fatal(err)
	}
	loc := time.UTC
	d := s.(schedule.DailyAt)
	d.Loc = loc

	// At noon, next firing is 4:30 the next day.
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, loc)
	got := d.Next(now)
	want := time.Date(2026, 4, 29, 4, 30, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("Next(noon) = %v, want %v", got, want)
	}

	// At 03:00, next firing is 04:30 today.
	now = time.Date(2026, 4, 28, 3, 0, 0, 0, loc)
	got = d.Next(now)
	want = time.Date(2026, 4, 28, 4, 30, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("Next(03:00) = %v, want %v", got, want)
	}

	// Exactly at 04:30 — Next must return the NEXT day, not the same instant.
	now = time.Date(2026, 4, 28, 4, 30, 0, 0, loc)
	got = d.Next(now)
	want = time.Date(2026, 4, 29, 4, 30, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("Next(equals firing) = %v, want next-day %v", got, want)
	}
}

// TestDailyAt_Next_DSTStable pins that a daily schedule keeps firing at
// the same LOCAL wall-clock time across a DST transition. The previous
// implementation rolled forward with a fixed candidate.Add(24h), which
// drifts an hour around spring-forward (a 23h day) / fall-back (a 25h
// day). Europe/Berlin springs forward on 2026-03-29 (02:00→03:00) and
// falls back on 2026-10-25 (03:00→02:00).
func TestDailyAt_Next_DSTStable(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("Europe/Berlin tzdata unavailable: %v", err)
	}
	// 04:30 is clear of both transitions' gap/overlap windows, so the
	// next firing must be EXACTLY 04:30 local on the following day —
	// not 03:30 or 05:30 from a 24h step crossing the boundary.
	d := schedule.DailyAt{Hour: 4, Minute: 30, Loc: berlin}

	cases := []struct {
		name      string
		afterY    int
		afterM    time.Month
		afterD    int
		wantNextD int
		wantNextM time.Month
	}{
		// 'after' is the evening BEFORE each transition day; the next
		// firing crosses the transition.
		{"across spring-forward", 2026, time.March, 28, 29, time.March},
		{"across fall-back", 2026, time.October, 24, 25, time.October},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			after := time.Date(c.afterY, c.afterM, c.afterD, 20, 0, 0, 0, berlin)
			got := d.Next(after)
			want := time.Date(c.afterY, c.wantNextM, c.wantNextD, 4, 30, 0, 0, berlin)
			if !got.Equal(want) {
				t.Errorf("Next = %s; want %s (must stay 04:30 local across DST)",
					got.In(berlin).Format(time.RFC3339), want.Format(time.RFC3339))
			}
			// And the hour/minute in local time must be exactly 04:30.
			loc := got.In(berlin)
			if loc.Hour() != 4 || loc.Minute() != 30 {
				t.Errorf("local firing = %02d:%02d; want 04:30", loc.Hour(), loc.Minute())
			}
		})
	}
}

func TestParse_At(t *testing.T) {
	s, err := schedule.Parse(schedule.Spec{At: "2026-04-28T09:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 4, 28, 8, 0, 0, 0, time.UTC)
	got := s.Next(now)
	want := time.Date(2026, 4, 28, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
	// After firing, Next returns zero.
	now = time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC)
	if got := s.Next(now); !got.IsZero() {
		t.Errorf("Next after firing = %v, want zero", got)
	}
}

func TestParse_RejectsEmpty(t *testing.T) {
	_, err := schedule.Parse(schedule.Spec{})
	if err == nil {
		t.Fatal("empty spec should reject")
	}
}

func TestParse_RejectsMultiple(t *testing.T) {
	_, err := schedule.Parse(schedule.Spec{Every: "6h", DailyAt: "04:00"})
	if err == nil {
		t.Fatal("multi-shape spec should reject")
	}
}

func TestParse_RejectsBadDuration(t *testing.T) {
	_, err := schedule.Parse(schedule.Spec{Every: "fortnight"})
	if err == nil {
		t.Fatal("bad duration should reject")
	}
}

func TestParse_RejectsTooFastEvery(t *testing.T) {
	_, err := schedule.Parse(schedule.Spec{Every: "100ms"})
	if err == nil {
		t.Fatal("sub-second should reject")
	}
}

func TestParse_RejectsBadDailyAt(t *testing.T) {
	cases := []string{"4", "04:60", "25:00", "ab:cd", "04-30"}
	for _, c := range cases {
		if _, err := schedule.Parse(schedule.Spec{DailyAt: c}); err == nil {
			t.Errorf("%q should reject", c)
		}
	}
}

// fakeClock is a deterministic clock for engine tests. After(d)
// returns a channel that fires immediately when wakeAll is called —
// so a test can advance time by issuing one Wake per scheduled tick.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	pending []chan time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	// Fire after every advance — tests call advance(d) to simulate.
	c.pending = append(c.pending, ch)
	_ = d
	return ch
}

// advance moves now forward and wakes everyone waiting on After.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	pending := c.pending
	c.pending = nil
	tickAt := c.now
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- tickAt
	}
}

func TestEngine_RunsScheduledTask(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)}
	e := schedule.New(schedule.WithClock(clock))

	var runs atomic.Int32
	if err := e.Add(&schedule.Task{
		Name:     "test",
		Schedule: schedule.Every{Interval: time.Hour},
		Run: func(ctx context.Context) error {
			runs.Add(1)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	// Advance time past the first firing.
	clock.advance(time.Hour)
	// Give the engine a moment to pick up.
	time.Sleep(50 * time.Millisecond)
	if got := runs.Load(); got != 1 {
		t.Errorf("after 1h, runs = %d, want 1", got)
	}

	clock.advance(time.Hour)
	time.Sleep(50 * time.Millisecond)
	if got := runs.Load(); got != 2 {
		t.Errorf("after 2h, runs = %d, want 2", got)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want context.Canceled", err)
	}
}

func TestEngine_TaskErrorDoesntStopEngine(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)}
	e := schedule.New(schedule.WithClock(clock))

	var runs atomic.Int32
	var lastErr atomic.Pointer[string]
	e = schedule.New(schedule.WithClock(clock), schedule.WithOnFinish(func(name string, due time.Time, dur time.Duration, err error) {
		if err != nil {
			s := err.Error()
			lastErr.Store(&s)
		}
	}))

	if err := e.Add(&schedule.Task{
		Name:     "boom",
		Schedule: schedule.Every{Interval: time.Hour},
		Run: func(_ context.Context) error {
			runs.Add(1)
			return errors.New("boom")
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	clock.advance(time.Hour)
	time.Sleep(50 * time.Millisecond)
	clock.advance(time.Hour)
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if got := runs.Load(); got < 2 {
		t.Errorf("expected engine to keep running after error; ran %d times", got)
	}
	got := lastErr.Load()
	if got == nil || *got != "boom" {
		t.Errorf("expected onFinish to receive error; got %v", got)
	}
}

func TestEngine_PanicRecovered(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)}
	e := schedule.New(schedule.WithClock(clock))

	finished := make(chan error, 4)
	e = schedule.New(schedule.WithClock(clock), schedule.WithOnFinish(func(name string, due time.Time, dur time.Duration, err error) {
		finished <- err
	}))

	if err := e.Add(&schedule.Task{
		Name:     "panicky",
		Schedule: schedule.Every{Interval: time.Hour},
		Run: func(_ context.Context) error {
			panic("kaboom")
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = e.Run(ctx) }()
	clock.advance(time.Hour)

	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "panicked") {
			t.Errorf("expected panic-recovery error; got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onFinish never fired after panicking task")
	}
	cancel()
}

func TestEngine_ZeroTasksReturnsErr(t *testing.T) {
	e := schedule.New()
	err := e.Run(context.Background())
	if !errors.Is(err, schedule.ErrNoTasks) {
		t.Errorf("expected ErrNoTasks; got %v", err)
	}
}

func TestEngine_RejectsBadTask(t *testing.T) {
	e := schedule.New()
	cases := []*schedule.Task{
		nil,
		{Name: "no-sched", Run: func(_ context.Context) error { return nil }},
		{Name: "no-run", Schedule: schedule.Every{Interval: time.Hour}},
		{Schedule: schedule.Every{Interval: time.Hour}, Run: func(_ context.Context) error { return nil }}, // empty Name
	}
	for _, tc := range cases {
		if err := e.Add(tc); err == nil {
			t.Errorf("Add(%v) should fail", tc)
		}
	}
}

func TestEngine_RejectsExpiredOnce(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)}
	e := schedule.New(schedule.WithClock(clock))
	pastSched, _ := schedule.Parse(schedule.Spec{At: "2020-01-01T00:00:00Z"})
	err := e.Add(&schedule.Task{
		Name:     "expired",
		Schedule: pastSched,
		Run:      func(_ context.Context) error { return nil },
	})
	if !errors.Is(err, schedule.ErrEmptySchedule) {
		t.Errorf("expected ErrEmptySchedule for past one-shot; got %v", err)
	}
}

func TestEngine_Jitter_SpreadsFirstFirings(t *testing.T) {
	// With jitter, two tasks scheduled with identical specs should
	// land at DIFFERENT first-firing instants (modulo extreme RNG
	// luck — we use a 1s jitter window and assert the firings are
	// not identical, which fails with probability ~0).
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: now}
	e := schedule.New(schedule.WithClock(clock), schedule.WithJitter(time.Second))

	for _, name := range []string{"a", "b", "c", "d"} {
		if err := e.Add(&schedule.Task{
			Name:     name,
			Schedule: schedule.Every{Interval: time.Hour},
			Run:      func(_ context.Context) error { return nil },
		}); err != nil {
			t.Fatal(err)
		}
	}

	tasks := e.Tasks()
	seen := map[time.Time]int{}
	for _, ts := range tasks {
		seen[ts.NextDue]++
	}
	if len(seen) < 2 {
		t.Errorf("jitter should spread first firings; got %d distinct NextDue values across %d tasks",
			len(seen), len(tasks))
	}
	// Every NextDue must fall in [now+1h, now+1h+jitter).
	earliest := now.Add(time.Hour)
	latest := earliest.Add(time.Second)
	for _, ts := range tasks {
		if ts.NextDue.Before(earliest) || !ts.NextDue.Before(latest) {
			t.Errorf("%s: NextDue %v outside [%v, %v)", ts.Name, ts.NextDue, earliest, latest)
		}
	}
}

func TestEngine_NoJitter_FirstFiringIsExact(t *testing.T) {
	// Without jitter, two identically-scheduled tasks fire at the
	// SAME first-firing instant — the cadence the operator declared.
	clock := &fakeClock{now: time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)}
	e := schedule.New(schedule.WithClock(clock))
	for _, name := range []string{"a", "b"} {
		_ = e.Add(&schedule.Task{
			Name:     name,
			Schedule: schedule.Every{Interval: time.Hour},
			Run:      func(_ context.Context) error { return nil },
		})
	}
	tasks := e.Tasks()
	if tasks[0].NextDue != tasks[1].NextDue {
		t.Errorf("without jitter the firings should match; got %v vs %v",
			tasks[0].NextDue, tasks[1].NextDue)
	}
}

func TestEngine_TasksSnapshot(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)}
	e := schedule.New(schedule.WithClock(clock))

	_ = e.Add(&schedule.Task{
		Name:     "backup:db1",
		Schedule: schedule.Every{Interval: 6 * time.Hour},
		Run:      func(_ context.Context) error { return nil },
	})
	_ = e.Add(&schedule.Task{
		Name:     "rotate:db1",
		Schedule: schedule.Every{Interval: 24 * time.Hour},
		Run:      func(_ context.Context) error { return nil },
	})

	tasks := e.Tasks()
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	for _, ts := range tasks {
		if ts.NextDue.IsZero() {
			t.Errorf("%s: NextDue zero", ts.Name)
		}
		if !strings.HasPrefix(ts.Description, "every ") {
			t.Errorf("%s: Description = %q", ts.Name, ts.Description)
		}
	}
}

// TestSchedule_NextStrictlyAdvancesOrZero pins the contract the engine
// relies on to never tight-loop (CPU-pathology audit #3): every concrete
// Schedule.Next(after) returns an instant STRICTLY after `after`, or the
// zero Time (a spent one-shot, which the engine prunes). If any Next
// could return `after` or earlier, the engine's "already due" branch —
// which has no sleep — would re-fire the task forever. Probes hit exact
// firing boundaries (where after == the candidate instant), the case
// most likely to regress into a non-advancing return.
func TestSchedule_NextStrictlyAdvancesOrZero(t *testing.T) {
	base := time.Date(2026, 4, 28, 2, 30, 0, 0, time.UTC)

	recurring := []schedule.Schedule{
		schedule.Every{Interval: time.Second}, // the parse-enforced minimum
		schedule.Every{Interval: time.Hour},
		schedule.DailyAt{Hour: 2, Minute: 30, Loc: time.UTC},
		schedule.DailyAt{Hour: 0, Minute: 0, Loc: time.UTC},
	}
	probes := []time.Time{
		base,
		base.Add(-time.Nanosecond),
		base.Add(time.Nanosecond),
		base.Add(11 * time.Hour),
		time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC), // midnight boundary
	}
	for _, s := range recurring {
		// Feed each schedule its own output to simulate the engine's
		// repeated Next(Next(...)) — a non-advancing step would stall here.
		cur := base
		for i := 0; i < 5; i++ {
			n := s.Next(cur)
			if !n.After(cur) {
				t.Fatalf("%s: Next(%v) = %v — must be strictly after", s.Description(), cur, n)
			}
			cur = n
		}
		for _, p := range probes {
			if n := s.Next(p); !n.After(p) {
				t.Errorf("%s: Next(%v) = %v — must be strictly after", s.Description(), p, n)
			}
		}
	}

	// Once: future → the instant; at/after the instant → zero.
	once := &schedule.Once{When: base}
	if n := once.Next(base.Add(-time.Hour)); !n.Equal(base) {
		t.Errorf("Once.Next(before) = %v, want %v", n, base)
	}
	if n := once.Next(base); !n.IsZero() {
		t.Errorf("Once.Next(==when) = %v, want zero", n)
	}
	if n := once.Next(base.Add(time.Hour)); !n.IsZero() {
		t.Errorf("Once.Next(after) = %v, want zero", n)
	}
}

// TestEngine_AlreadyDueDoesNotSpin drives a task through the engine's
// "already due" (wait <= 0) branch and asserts a SINGLE clock advance
// yields exactly ONE run, then the engine blocks waiting for the next
// tick — it does not re-fire without time moving. This is the
// engine-level corollary of the strict-advance contract: because
// runOne resets nextDue to Schedule.Next(now) (strictly future), the
// no-sleep already-due branch can't loop. Revert-verified: make
// Every.Next return `after` unchanged and this spins (runs >> 1).
func TestEngine_AlreadyDueDoesNotSpin(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)}
	e := schedule.New(schedule.WithClock(clock))

	var runs atomic.Int32
	if err := e.Add(&schedule.Task{
		Name:     "fast",
		Schedule: schedule.Every{Interval: time.Second},
		Run: func(ctx context.Context) error {
			runs.Add(1)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	// One advance past the first firing. The engine wakes with
	// dueAt == now (wait == 0) → the already-due branch runs the task
	// once, then nextDue jumps to now+1s and it blocks on the next tick.
	clock.advance(time.Second)
	time.Sleep(100 * time.Millisecond)
	if got := runs.Load(); got != 1 {
		t.Fatalf("after one tick runs = %d, want exactly 1 (a non-advancing next-due would spin)", got)
	}

	// A second advance → exactly one more run, proving each run needs
	// the clock to move.
	clock.advance(time.Second)
	time.Sleep(100 * time.Millisecond)
	if got := runs.Load(); got != 2 {
		t.Fatalf("after two ticks runs = %d, want exactly 2", got)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want context.Canceled", err)
	}
}

// pollNextDue waits until the single task's NextDue moves off `initial`
// (i.e. runOne has recomputed it) and returns it.
func pollNextDue(t *testing.T, e *schedule.Engine, initial time.Time) time.Time {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ts := e.Tasks()
		if len(ts) == 1 && !ts[0].NextDue.Equal(initial) {
			return ts[0].NextDue
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("NextDue never advanced off %v", initial)
	return time.Time{}
}

// TestEngine_EveryStaysOnGridWithSlowTask pins the drift fix: an Every
// task whose run takes time must schedule its next firing relative to the
// SCHEDULED slot, not the completion time. Computing Next(completion)
// pushed every cycle later by the run duration, silently under-running the
// configured cadence.
func TestEngine_EveryStaysOnGridWithSlowTask(t *testing.T) {
	t0 := time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: t0}
	e := schedule.New(schedule.WithClock(clock))

	if err := e.Add(&schedule.Task{
		Name:     "every1h",
		Schedule: schedule.Every{Interval: time.Hour},
		Run: func(ctx context.Context) error {
			clock.advance(10 * time.Minute) // a 10-minute run
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = e.Run(ctx) }()
	time.Sleep(50 * time.Millisecond) // let Run register its first After

	clock.advance(time.Hour) // reach the first firing at t0+1h; the task runs

	got := pollNextDue(t, e, t0.Add(time.Hour))
	want := t0.Add(2 * time.Hour) // grid-anchored; the drift bug yields t0+2h10m
	if !got.Equal(want) {
		t.Errorf("next fire = %v, want %v (grid-anchored); drift = %v", got, want, got.Sub(want))
	}
}

// TestEngine_EveryCatchesUpAfterOverrun: when a run overruns one or more
// whole intervals, the next firing skips to the first FUTURE grid slot
// (no back-to-back "make-up" pile-up), still anchored to the grid.
func TestEngine_EveryCatchesUpAfterOverrun(t *testing.T) {
	t0 := time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: t0}
	e := schedule.New(schedule.WithClock(clock))

	if err := e.Add(&schedule.Task{
		Name:     "every1h",
		Schedule: schedule.Every{Interval: time.Hour},
		Run: func(ctx context.Context) error {
			clock.advance(2*time.Hour + 30*time.Minute) // overruns 2 whole intervals
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = e.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	clock.advance(time.Hour) // fire at t0+1h; run finishes at t0+3h30m

	got := pollNextDue(t, e, t0.Add(time.Hour))
	want := t0.Add(4 * time.Hour) // skipped the t0+2h and t0+3h slots; first future slot
	if !got.Equal(want) {
		t.Errorf("next fire = %v, want %v (first future grid slot)", got, want)
	}
}
