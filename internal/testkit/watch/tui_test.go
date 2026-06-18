package watch_test

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/watch"
)

// TestModel_View_RendersCellsAndTail covers the load-bearing
// rendering path: after feeding a sequence of events through
// Update, the View output contains every cell name, current
// iteration, backup counters, and the most-recent event in
// the tail.  Doesn't assert exact byte layout — that's brittle
// against future styling tweaks — only that the data we care
// about made it through.
func TestModel_View_RendersCellsAndTail(t *testing.T) {
	m := watch.NewModel("./test-runs/run-test")
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = tm.(*watch.Model)

	// Feed a representative sequence.
	events := []watch.Event{
		{At: ts("10:00:00"), Cell: "alpha", Op: "setup_started"},
		{At: ts("10:00:05"), Cell: "alpha", Op: "setup_ok"},
		{At: ts("10:00:10"), Cell: "alpha", Op: "iter_start", Iteration: 1},
		{At: ts("10:00:11"), Cell: "alpha", Op: "backup_started", Iteration: 1},
		{At: ts("10:00:20"), Cell: "alpha", Op: "backup_completed", Iteration: 1},
		{At: ts("10:00:25"), Cell: "bravo", Op: "iter_start", Iteration: 3},
		{At: ts("10:00:30"), Cell: "bravo", Op: "fault_apply", Iteration: 3,
			Detail: "signal(target=pg, sig=9)"},
	}
	for _, ev := range events {
		tm, _ := m.Update(toMsg(ev))
		m = tm.(*watch.Model)
	}

	view := m.View()

	// Cells.  Both names appear; backup counter "1/1" for alpha.
	for _, want := range []string{"alpha", "bravo", "1/1"} {
		if !strings.Contains(stripANSI(view), want) {
			t.Errorf("View missing %q\n--- view ---\n%s", want, view)
		}
	}
	// Tail shows the most recent fault.
	if !strings.Contains(stripANSI(view), "signal(target=pg, sig=9)") {
		t.Errorf("View tail missing the fault detail\n--- view ---\n%s", view)
	}
	// Stats line: events count + cells count.
	if !strings.Contains(stripANSI(view), "events: 7") {
		t.Errorf("View stats missing event count\n--- view ---\n%s", view)
	}
	if !strings.Contains(stripANSI(view), "cells: 2") {
		t.Errorf("View stats missing cell count\n--- view ---\n%s", view)
	}
}

// TestModel_View_PreSizeShowsLoading covers the bubbletea
// startup race: WindowSizeMsg is delivered AFTER Init, so the
// first View() call must not panic on width=0.
func TestModel_View_PreSizeShowsLoading(t *testing.T) {
	m := watch.NewModel("/some/path")
	out := m.View()
	if !strings.Contains(out, "loading") {
		t.Errorf("pre-size View should show loading; got %q", out)
	}
}

// TestModel_QuitKeysReturnQuit locks the user-quit path.  q,
// esc, and ctrl+c all return tea.Quit so the watcher exits
// cleanly without leaving the operator stuck in alt-screen.
func TestModel_QuitKeysReturnQuit(t *testing.T) {
	for _, key := range []string{"q", "esc", "ctrl+c"} {
		t.Run(key, func(t *testing.T) {
			m := watch.NewModel("/p")
			_, cmd := m.Update(tea.KeyMsg{Type: keyType(key), Runes: []rune(key)})
			if cmd == nil {
				t.Errorf("%s should return a Cmd (Quit)", key)
			}
			// We can't easily assert cmd == tea.Quit (it's a
			// func), but we CAN execute it and look for the
			// QuitMsg sentinel.
			msg := cmd()
			if _, isQuit := msg.(tea.QuitMsg); !isQuit {
				t.Errorf("%s expected Quit; got %T", key, msg)
			}
		})
	}
}

// --- test helpers -----------------------------------------------------

func ts(hms string) time.Time {
	t, err := time.Parse("15:04:05", hms)
	if err != nil {
		panic(err)
	}
	return t
}

// toMsg routes an Event into the bubbletea Update via the
// internal eventMsg type.  Since eventMsg is unexported, we
// thunk through bubbletea's Send-equivalent: feeding a value
// of the correct underlying type works because Update's type
// switch matches by concrete type.  We use the public Apply
// path here instead — feed through a dummy *Model field.
//
// In practice, the watch.Model's Update accepts eventMsg via
// the type switch.  Since the _test package can't reach the
// unexported type, we exercise the same code path through a
// thin export below (see watch/export_test.go).
func toMsg(ev watch.Event) tea.Msg {
	return watch.EventMsgForTest(ev)
}

// keyType maps a key string back to a tea.KeyType for
// constructing test KeyMsgs.  Only the three quit keys.
func keyType(s string) tea.KeyType {
	switch s {
	case "esc":
		return tea.KeyEsc
	case "ctrl+c":
		return tea.KeyCtrlC
	default:
		return tea.KeyRunes
	}
}

// stripANSI removes ANSI escape sequences so substring asserts
// don't trip on lipgloss colour wrappers.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if r == 0x1b {
			inEsc = true
			continue
		}
		if inEsc {
			if r == 'm' || r == 'K' || r == 'H' {
				inEsc = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
