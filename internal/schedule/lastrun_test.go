package schedule_test

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/schedule"
)

func firstDueWith(t *testing.T, store schedule.LastRunStore, now time.Time, s schedule.Schedule) (time.Time, error) {
	t.Helper()
	e := schedule.New(schedule.WithClock(&fakeClock{now: now}), schedule.WithLastRunStore(store))
	err := e.Add(&schedule.Task{Name: "t", Schedule: s, Run: func(context.Context) error { return nil }})
	if err != nil {
		return time.Time{}, err
	}
	return e.Tasks()[0].NextDue, nil
}

func openStore(t *testing.T, path string) *schedule.FileLastRunStore {
	t.Helper()
	s, err := schedule.OpenFileLastRunStore(path, func(err error) { t.Errorf("store write: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A restart must resume an `every` task's cadence from its recorded
// last run; a missed slot runs immediately; a never-run task is due now.
func TestEngine_LastRunStore_Every(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	every := schedule.Every{Interval: 6 * time.Hour}
	path := filepath.Join(t.TempDir(), "state.json")

	s := openStore(t, path)
	if got, _ := firstDueWith(t, s, now, every); !got.Equal(now) {
		t.Errorf("never run: due %v, want now %v", got, now)
	}

	s.RecordRun("t", now.Add(-time.Hour))
	s = openStore(t, path) // reload from disk, as a restart does
	if got, _ := firstDueWith(t, s, now, every); !got.Equal(now.Add(5 * time.Hour)) {
		t.Errorf("ran 1h ago: due %v, want %v", got, now.Add(5*time.Hour))
	}

	s.RecordRun("t", now.Add(-7*time.Hour))
	if got, _ := firstDueWith(t, s, now, every); !got.Equal(now) {
		t.Errorf("slot missed while down: due %v, want now", got)
	}
}

// daily_at keeps its declared slot on a first start, and catches up a
// slot missed while down.
func TestEngine_LastRunStore_DailyAt(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	daily := schedule.DailyAt{Hour: 4, Minute: 0, Loc: time.UTC}
	s := openStore(t, filepath.Join(t.TempDir(), "state.json"))

	if got, _ := firstDueWith(t, s, now, daily); !got.Equal(time.Date(2026, 5, 2, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("daily never run: due %v, want tomorrow 04:00", got)
	}
	s.RecordRun("t", time.Date(2026, 4, 30, 4, 0, 0, 0, time.UTC))
	if got, _ := firstDueWith(t, s, now, daily); !got.Equal(now) {
		t.Errorf("daily missed today's 04:00: due %v, want now", got)
	}
}

// Every run is recorded, so the next process picks up from it.
func TestEngine_LastRunStore_RecordsRuns(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: now}
	path := filepath.Join(t.TempDir(), "state.json")
	s := openStore(t, path)
	e := schedule.New(schedule.WithClock(clock), schedule.WithLastRunStore(s))
	var runs atomic.Int32
	if err := e.Add(&schedule.Task{Name: "t", Schedule: schedule.Every{Interval: time.Hour},
		Run: func(context.Context) error { runs.Add(1); return nil }}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for runs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if runs.Load() == 0 {
		t.Fatal("task never ran")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	got, ok := openStore(t, path).LastRun("t")
	if !ok || !got.Equal(now) {
		t.Errorf("recorded last run = %v,%v; want %v", got, ok, now)
	}
}

// A corrupt state file is reported but yields a usable empty store.
func TestOpenFileLastRunStore_Corrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := schedule.OpenFileLastRunStore(path, nil)
	if err == nil {
		t.Error("expected decode error")
	}
	if s == nil {
		t.Fatal("store must be usable despite the error")
	}
	if _, ok := s.LastRun("t"); ok {
		t.Error("corrupt store should be empty")
	}
}
