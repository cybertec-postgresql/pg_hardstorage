// tui.go — bubbletea TUI: live soak dashboard with per-cell status + recent-event footer.
package watch

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// tickInterval is how often the model recomputes its rendering
// (cheap — pure func of state) and refreshes the elapsed clock.
// 500 ms is enough to feel live without burning cycles on a
// 50-cell soak.
const tickInterval = 500 * time.Millisecond

// recentTailLines is how many events the footer shows.  Wide
// enough to see a fault → recovery → next-iter cycle, narrow
// enough to fit on a typical SSH'd tmux pane.
const recentTailLines = 8

// styles — kept to a small palette so a degraded terminal
// (no colour, basic ANSI) still renders cleanly.
var (
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")) // bright blue
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))             // grey
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))            // bright green
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))            // bright yellow
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))             // bright red
)

// eventMsg is delivered to the bubbletea Update loop every time
// the follower decodes a new event.  The Update handler folds
// it into State.
type eventMsg Event

// tickMsg is the periodic redraw heartbeat.
type tickMsg time.Time

// followerErrMsg surfaces a fatal follower error to the model
// so the UI can show it instead of silently freezing.
type followerErrMsg struct{ err error }

// Model is the bubbletea state.  Public so a future caller
// (e.g. an embedded mode in `validate`) could construct it
// without going through the watch command.
type Model struct {
	state     *State
	path      string
	startedAt time.Time
	width     int
	height    int
	followErr error
}

// NewModel constructs a fresh Model bound to path.  The follower
// goroutine is started by Init() so the bubbletea program owns
// the lifecycle.
func NewModel(path string) *Model {
	return &Model{
		state:     New(0),
		path:      path,
		startedAt: time.Now(),
	}
}

// Run is the convenience entrypoint the cobra command uses.
// Spawns the follower, runs bubbletea in alt-screen, and tears
// everything down on quit / error.
func Run(ctx context.Context, path string) error {
	m := NewModel(path)
	p := tea.NewProgram(m,
		tea.WithAltScreen(),
		tea.WithContext(ctx),
		tea.WithMouseCellMotion(),
	)

	// Pump events into the program.  The Cmd returned by
	// followCmd does the actual work; this just wires the
	// goroutine's events as Send messages.
	go func() {
		_ = Follow(ctx, path, FollowOptions{FromBeginning: true},
			func(ev Event) { p.Send(eventMsg(ev)) })
	}()

	_, err := p.Run()
	return err
}

// Init kicks off the periodic tick.  The follower goroutine is
// started by Run() (one process-level pump rather than per-Init,
// because Init can be called multiple times in some flows).
func (m *Model) Init() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Update is the bubbletea reducer.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		}
	case eventMsg:
		m.state.Apply(Event(msg))
	case followerErrMsg:
		m.followErr = msg.err
	case tickMsg:
		return m, tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
	}
	return m, nil
}

// View renders the full screen.  Pure function of state +
// dimensions; no I/O.  Layout (top to bottom):
//
//	┌────────────────────────────────────────────────┐
//	│ pg_hardstorage watch — <path>                  │  ← header (1 line)
//	│ events: 1234 · started 12:34 · elapsed 47m12s  │  ← stats (1 line)
//	│                                                │  ← spacer
//	│ CELL                OS    PG  ITER ...         │  ← cells header
//	│ debian-12-pg15      d:12  15  47   ...         │  ← cell row(s)
//	│ ...                                            │
//	│                                                │  ← spacer
//	│ recent events ─────────────────────────────    │  ← tail header
//	│ 12:34:56  alpha     iter_start                 │  ← tail rows
//	│ ...                                            │
//	│ q · esc · ctrl-c to quit                       │  ← footer hint
//	└────────────────────────────────────────────────┘
func (m *Model) View() string {
	if m.width == 0 {
		// Bubbletea hasn't sent the initial WindowSizeMsg yet.
		return "loading…"
	}
	snap := m.state.Snapshot()
	var b strings.Builder
	b.WriteString(m.renderHeader(snap))
	b.WriteByte('\n')
	b.WriteString(m.renderStats(snap))
	b.WriteString("\n\n")
	b.WriteString(m.renderCellsTable(snap))
	b.WriteString("\n\n")
	b.WriteString(m.renderTail(snap))
	b.WriteByte('\n')
	b.WriteString(dimStyle.Render("q · esc · ctrl-c to quit"))
	return b.String()
}

func (m *Model) renderHeader(snap Snapshot) string {
	title := headerStyle.Render("pg_hardstorage watch")
	return fmt.Sprintf("%s — %s", title, dimStyle.Render(m.path))
}

func (m *Model) renderStats(snap Snapshot) string {
	var elapsed string
	if !snap.FirstAt.IsZero() {
		elapsed = snap.LastAt.Sub(snap.FirstAt).Round(time.Second).String()
	} else {
		elapsed = "—"
	}
	parts := []string{
		fmt.Sprintf("events: %d", snap.TotalEvents),
		fmt.Sprintf("cells: %d", len(snap.Cells)),
		"span: " + elapsed,
		"watcher: " + time.Since(m.startedAt).Round(time.Second).String(),
	}
	stats := strings.Join(parts, " · ")
	if m.followErr != nil {
		stats += "  " + errStyle.Render("(follow error: "+m.followErr.Error()+")")
	}
	return dimStyle.Render(stats)
}

func (m *Model) renderCellsTable(snap Snapshot) string {
	if len(snap.Cells) == 0 {
		return dimStyle.Render("(no cells reporting yet — waiting for events)")
	}
	// Fixed-width columns sized for typical fleet rows.  We
	// don't auto-size based on terminal width; that's a level
	// of polish we don't need today and complicates testing.
	const (
		cellW = 28
		iterW = 5
		bkupW = 11 // "ok/total"
		fltW  = 6
		opW   = 22
	)
	var b strings.Builder
	header := fmt.Sprintf("%-*s %-*s %-*s %-*s %s",
		cellW, "CELL",
		iterW, "ITER",
		bkupW, "BACKUPS",
		fltW, "FAULTS",
		"LAST OP / ERR")
	b.WriteString(headerStyle.Render(header))
	b.WriteByte('\n')
	for _, c := range snap.Cells {
		b.WriteString(m.renderCellRow(c, cellW, iterW, bkupW, fltW, opW))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *Model) renderCellRow(c CellState, cellW, iterW, bkupW, fltW, opW int) string {
	cellName := truncate(c.Name, cellW)
	iter := fmt.Sprintf("%d", c.Iteration)
	backups := fmt.Sprintf("%d/%d", c.BackupsOK, c.BackupsStarted)
	faults := fmt.Sprintf("%d", c.FaultsApplied)

	last := c.LastOp
	if c.LastErr != "" {
		last = errStyle.Render("✗ " + collapseSpaces(c.LastErr))
	} else if c.LastOp == "backup_completed" || c.LastOp == "verify_ok" || c.LastOp == "fault_recovered" {
		last = okStyle.Render(c.LastOp)
	} else if c.LastOp == "fault_apply" {
		last = warnStyle.Render(c.LastOp + " " + truncate(c.LastDetail, 40))
	}
	return fmt.Sprintf("%-*s %-*s %-*s %-*s %s",
		cellW, cellName,
		iterW, iter,
		bkupW, backups,
		fltW, faults,
		last)
}

func (m *Model) renderTail(snap Snapshot) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("RECENT EVENTS"))
	b.WriteByte('\n')
	if len(snap.RecentTail) == 0 {
		b.WriteString(dimStyle.Render("(no events yet)"))
		return b.String()
	}
	// Show the last N events; oldest first so the most recent
	// is at the bottom (the operator's eye lands there).
	start := 0
	if len(snap.RecentTail) > recentTailLines {
		start = len(snap.RecentTail) - recentTailLines
	}
	for _, ev := range snap.RecentTail[start:] {
		ts := ev.At.Format("15:04:05")
		cell := ev.Cell
		if cell == "" {
			cell = "—"
		}
		summary := SummariseDetail(ev)
		b.WriteString(fmt.Sprintf("%s  %-22s  %s\n",
			dimStyle.Render(ts), truncate(cell, 22), summary))
	}
	return strings.TrimRight(b.String(), "\n")
}

// truncate clips s to maxLen runes with an ellipsis if it
// exceeds.  Plain rune-slice truncation rather than ANSI-aware
// because we don't pass styled strings through here.
func truncate(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	if maxLen <= 1 {
		return string(r[:maxLen])
	}
	return string(r[:maxLen-1]) + "…"
}
