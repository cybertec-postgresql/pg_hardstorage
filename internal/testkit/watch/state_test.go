package watch_test

import (
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/watch"
)

func mkEvent(cell, op string, opts ...func(*watch.Event)) watch.Event {
	ev := watch.Event{Cell: cell, Op: op, At: time.Now()}
	for _, o := range opts {
		o(&ev)
	}
	return ev
}

func withIter(n int) func(*watch.Event)      { return func(e *watch.Event) { e.Iteration = n } }
func withDetail(d string) func(*watch.Event) { return func(e *watch.Event) { e.Detail = d } }
func withErr(e string) func(*watch.Event)    { return func(ev *watch.Event) { ev.Err = e } }
func withAt(t time.Time) func(*watch.Event)  { return func(e *watch.Event) { e.At = t } }

func TestState_AggregatesPerCellCounters(t *testing.T) {
	s := watch.New(0)
	s.Apply(mkEvent("alpha", "setup_started"))
	s.Apply(mkEvent("alpha", "setup_ok"))
	s.Apply(mkEvent("alpha", "iter_start", withIter(1)))
	s.Apply(mkEvent("alpha", "backup_started", withIter(1)))
	s.Apply(mkEvent("alpha", "backup_completed", withIter(1)))
	s.Apply(mkEvent("alpha", "fault_apply", withIter(2),
		withDetail("signal(target=pg, sig=9)")))
	s.Apply(mkEvent("alpha", "verify_ok", withIter(3)))
	s.Apply(mkEvent("alpha", "backup_failed", withIter(4),
		withErr("disk full")))

	snap := s.Snapshot()
	if len(snap.Cells) != 1 {
		t.Fatalf("expected 1 cell; got %d", len(snap.Cells))
	}
	c := snap.Cells[0]
	if c.Iteration != 4 {
		t.Errorf("Iteration = %d, want 4 (max)", c.Iteration)
	}
	if c.BackupsStarted != 1 {
		t.Errorf("BackupsStarted = %d, want 1", c.BackupsStarted)
	}
	if c.BackupsOK != 1 {
		t.Errorf("BackupsOK = %d, want 1", c.BackupsOK)
	}
	if c.BackupsFailed != 1 {
		t.Errorf("BackupsFailed = %d, want 1", c.BackupsFailed)
	}
	if c.VerifiesOK != 1 {
		t.Errorf("VerifiesOK = %d, want 1", c.VerifiesOK)
	}
	if c.FaultsApplied != 1 {
		t.Errorf("FaultsApplied = %d, want 1", c.FaultsApplied)
	}
	if c.LastOp != "backup_failed" {
		t.Errorf("LastOp = %q, want backup_failed", c.LastOp)
	}
	if c.LastErr != "disk full" {
		t.Errorf("LastErr = %q, want 'disk full'", c.LastErr)
	}
	if snap.TotalEvents != 8 {
		t.Errorf("TotalEvents = %d, want 8", snap.TotalEvents)
	}
}

func TestState_SortsCellsByName(t *testing.T) {
	s := watch.New(0)
	for _, name := range []string{"zebra", "alpha", "mike"} {
		s.Apply(mkEvent(name, "iter_start"))
	}
	snap := s.Snapshot()
	if len(snap.Cells) != 3 {
		t.Fatalf("expected 3 cells; got %d", len(snap.Cells))
	}
	if snap.Cells[0].Name != "alpha" || snap.Cells[1].Name != "mike" || snap.Cells[2].Name != "zebra" {
		t.Errorf("cells not sorted: %v", []string{snap.Cells[0].Name, snap.Cells[1].Name, snap.Cells[2].Name})
	}
}

func TestState_TailRingBoundsMemory(t *testing.T) {
	s := watch.New(5)
	for i := 0; i < 20; i++ {
		s.Apply(mkEvent("c", "iter_start", withIter(i)))
	}
	snap := s.Snapshot()
	if len(snap.RecentTail) != 5 {
		t.Errorf("tail len = %d, want 5", len(snap.RecentTail))
	}
	// Last item is the most recent.
	if snap.RecentTail[4].Iteration != 19 {
		t.Errorf("tail[4].Iteration = %d, want 19", snap.RecentTail[4].Iteration)
	}
	// First item is iteration 15 (20 - 5).
	if snap.RecentTail[0].Iteration != 15 {
		t.Errorf("tail[0].Iteration = %d, want 15", snap.RecentTail[0].Iteration)
	}
	if snap.TotalEvents != 20 {
		t.Errorf("TotalEvents = %d, want 20", snap.TotalEvents)
	}
}

func TestState_FailedFlagOnSetupFailed(t *testing.T) {
	s := watch.New(0)
	s.Apply(mkEvent("dead-cell", "setup_failed", withErr("PG never started")))
	snap := s.Snapshot()
	if !snap.Cells[0].Failed {
		t.Errorf("Failed flag should be set after setup_failed")
	}
}

func TestState_FirstAndLastAtBoundsTimespan(t *testing.T) {
	s := watch.New(0)
	earliest := time.Date(2026, 5, 6, 10, 0, 0, 0, time.UTC)
	latest := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	mid := earliest.Add(time.Hour)
	// Apply OUT OF ORDER to make sure we track min/max, not
	// first/last-applied.
	s.Apply(mkEvent("c", "iter_start", withAt(mid)))
	s.Apply(mkEvent("c", "iter_start", withAt(latest)))
	s.Apply(mkEvent("c", "iter_start", withAt(earliest)))
	snap := s.Snapshot()
	if !snap.FirstAt.Equal(earliest) {
		t.Errorf("FirstAt = %v, want %v", snap.FirstAt, earliest)
	}
	if !snap.LastAt.Equal(latest) {
		t.Errorf("LastAt = %v, want %v", snap.LastAt, latest)
	}
}

func TestState_CelllessEventsCountInTotalsButNotInGrid(t *testing.T) {
	// Some scenario-runner events have empty Cell.  They should
	// still flow into TotalEvents + the tail, but not create a
	// ghost row in the cells grid.
	s := watch.New(0)
	s.Apply(mkEvent("", "scenario.started", withDetail("L4_patroni")))
	s.Apply(mkEvent("", "topology.up.starting"))
	s.Apply(mkEvent("real-cell", "iter_start"))
	snap := s.Snapshot()
	if len(snap.Cells) != 1 || snap.Cells[0].Name != "real-cell" {
		t.Errorf("cell-less events leaked into the grid: %v", snap.Cells)
	}
	if snap.TotalEvents != 3 {
		t.Errorf("TotalEvents = %d, want 3", snap.TotalEvents)
	}
}

func TestState_SnapshotIsDefensiveCopy(t *testing.T) {
	// Mutating the snapshot must not affect future Apply calls.
	s := watch.New(0)
	s.Apply(mkEvent("c", "iter_start"))
	snap := s.Snapshot()
	snap.Cells[0].Iteration = 9999
	snap.RecentTail[0].Op = "MUTATED"
	// New snapshot still reflects truth.
	snap2 := s.Snapshot()
	if snap2.Cells[0].Iteration == 9999 {
		t.Errorf("State.Snapshot returned a non-defensive copy of Cells")
	}
	if snap2.RecentTail[0].Op == "MUTATED" {
		t.Errorf("State.Snapshot returned a non-defensive copy of RecentTail")
	}
}

func TestSummariseDetail_ErrorBeatsDetail(t *testing.T) {
	out := watch.SummariseDetail(watch.Event{
		Op: "backup_failed", Err: "disk full", Detail: "ignored",
	})
	if !strings.HasPrefix(out, "✗ ") || !strings.Contains(out, "disk full") {
		t.Errorf("err should win + carry the ✗ marker; got %q", out)
	}
}

func TestSummariseDetail_DetailWhenNoErr(t *testing.T) {
	out := watch.SummariseDetail(watch.Event{
		Op: "fault_apply", Detail: "signal(target=pg, sig=9)",
	})
	if out != "signal(target=pg, sig=9)" {
		t.Errorf("detail passthrough; got %q", out)
	}
}

func TestSummariseDetail_OpFallback(t *testing.T) {
	out := watch.SummariseDetail(watch.Event{Op: "iter_start"})
	if out != "iter_start" {
		t.Errorf("op fallback; got %q", out)
	}
}

func TestSummariseDetail_CollapsesNewlinesToSpaces(t *testing.T) {
	multi := "first line\nsecond line\n\tthird"
	out := watch.SummariseDetail(watch.Event{Err: multi})
	if strings.Contains(out, "\n") || strings.Contains(out, "\t") {
		t.Errorf("multiline whitespace should collapse; got %q", out)
	}
	if !strings.Contains(out, "first line second line third") {
		t.Errorf("collapse should leave words intact; got %q", out)
	}
}
