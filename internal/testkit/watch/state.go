// Package watch is the live-view layer for soak + scenario
// runs.  It consumes the NDJSON event stream the orchestrator
// emits to <report-dir>/events.ndjson and aggregates it into a
// per-cell snapshot the TUI renders.
//
// Three layers, each independently testable:
//
//   - state: pure aggregation.  Feed Events in; read a Snapshot
//     out.  No I/O, no goroutines, no globals — easy to unit-test
//     against synthetic event sequences.
//   - follower: a tail/poll wrapper that streams new lines from
//     a file as the soak appends to it.
//   - tui: bubbletea model that renders Snapshots.  Headless
//     mode (no TTY / --no-tty) prints events as plain NDJSON.
//
// Why a side-process design (`testkit watch <run-dir>` in a
// second terminal) rather than embedding the TUI in the soak
// command itself: the soak's existing stdout NDJSON is already
// the canonical CI / scripting interface, and pushing a TUI
// onto stdout would break every CI pipeline that pipes through
// jq.  Side-process keeps the data path unchanged and lets
// operators attach / detach the live-view at will.
package watch

import (
	"sort"
	"strings"
	"time"
)

// Event mirrors validate.Event but without depending on the
// validate package — keeping watch a leaf import.  Fields
// match the on-disk JSON exactly.
type Event struct {
	At        time.Time `json:"at"`
	Cell      string    `json:"cell"`
	Iteration int       `json:"iteration,omitempty"`
	Op        string    `json:"op"`
	Detail    string    `json:"detail,omitempty"`
	Err       string    `json:"err,omitempty"`
}

// CellState is the rolled-up state of one cell.  Exposed so
// the TUI's table renderer can consume it directly without
// reimplementing aggregation.
type CellState struct {
	Name           string
	Iteration      int
	BackupsStarted int
	BackupsOK      int
	BackupsFailed  int
	VerifiesOK     int
	VerifiesFailed int
	FaultsApplied  int
	LastOp         string
	LastDetail     string
	LastErr        string
	LastUpdate     time.Time
	Failed         bool // setup_failed or terminal cell-failure observed
}

// Snapshot is the read-only view the TUI consumes.  Returned
// as a value so the renderer can hold it without locking the
// State.
type Snapshot struct {
	Cells       []CellState // sorted by Name for stable rendering
	TotalEvents int
	FirstAt     time.Time
	LastAt      time.Time
	RecentTail  []Event // bounded; oldest → newest
}

// State aggregates events into a Snapshot.  Single-goroutine —
// the follower feeds it, the renderer reads via Snapshot().
// Thread-safety is the caller's job; the watch command owns
// both feed and read on a single bubbletea goroutine.
type State struct {
	cells       map[string]*CellState
	totalEvents int
	firstAt     time.Time
	lastAt      time.Time
	tail        []Event // ring-buffered to TailMax events
	tailMax     int
}

// New returns an empty State.  tailMax bounds the recent-events
// ring shown in the TUI footer; 200 is enough for a few minutes
// of soak detail without unbounded memory growth.
func New(tailMax int) *State {
	if tailMax <= 0 {
		tailMax = 200
	}
	return &State{
		cells:   map[string]*CellState{},
		tailMax: tailMax,
	}
}

// Apply folds one Event into the rolling state.  Order matters
// for the LastOp field but not for counters (idempotent counts).
// Unknown Ops still bump LastOp / LastUpdate so a future
// orchestrator addition shows up in the watcher even before the
// renderer learns about it explicitly.
func (s *State) Apply(ev Event) {
	s.totalEvents++
	if s.firstAt.IsZero() || ev.At.Before(s.firstAt) {
		s.firstAt = ev.At
	}
	if ev.At.After(s.lastAt) {
		s.lastAt = ev.At
	}
	s.tail = append(s.tail, ev)
	if len(s.tail) > s.tailMax {
		s.tail = s.tail[len(s.tail)-s.tailMax:]
	}

	// Cell-less events (some scenario-runner shapes don't carry
	// a cell field) still feed the tail + counters but skip the
	// per-cell rollup.
	if ev.Cell == "" {
		return
	}
	c, ok := s.cells[ev.Cell]
	if !ok {
		c = &CellState{Name: ev.Cell}
		s.cells[ev.Cell] = c
	}
	if ev.Iteration > c.Iteration {
		c.Iteration = ev.Iteration
	}
	c.LastOp = ev.Op
	c.LastDetail = ev.Detail
	c.LastErr = ev.Err
	c.LastUpdate = ev.At

	switch ev.Op {
	case "setup_failed":
		c.Failed = true
	case "backup_started":
		c.BackupsStarted++
	case "backup_completed":
		c.BackupsOK++
	case "backup_failed":
		c.BackupsFailed++
	case "verify_ok":
		c.VerifiesOK++
	case "verify_failed":
		c.VerifiesFailed++
	case "fault_apply":
		c.FaultsApplied++
	}
}

// Snapshot returns a value-typed read-only view.  Cells are
// copied so the caller can't mutate State accidentally; the
// tail slice is also defensively copied because bubbletea's
// model passes Snapshot through Cmd channels.
func (s *State) Snapshot() Snapshot {
	cells := make([]CellState, 0, len(s.cells))
	for _, c := range s.cells {
		cells = append(cells, *c)
	}
	sort.Slice(cells, func(i, j int) bool { return cells[i].Name < cells[j].Name })
	tail := make([]Event, len(s.tail))
	copy(tail, s.tail)
	return Snapshot{
		Cells:       cells,
		TotalEvents: s.totalEvents,
		FirstAt:     s.firstAt,
		LastAt:      s.lastAt,
		RecentTail:  tail,
	}
}

// SummariseDetail returns a short, single-line rendering of an
// event's detail/err/op for the TUI's tail panel.  Errors win
// over details (operators looking at a soak care about what
// went wrong); both fall back to op when neither is set.
func SummariseDetail(ev Event) string {
	if ev.Err != "" {
		return "✗ " + collapseSpaces(ev.Err)
	}
	if ev.Detail != "" {
		return collapseSpaces(ev.Detail)
	}
	return ev.Op
}

// collapseSpaces folds runs of whitespace + newlines into single
// spaces so a multi-line error from `docker logs` collapses to
// one TUI line.
func collapseSpaces(s string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return strings.TrimSpace(b.String())
}
