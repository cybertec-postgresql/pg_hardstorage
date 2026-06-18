package simple

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/simple/prompt"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/simple/state"
)

// driveEnv builds a fully-stubbed Env that the dispatch loop can run
// against without touching any real PG / repo / disk-state.  Tests
// supply the canned stdin script + assert on the captured stdout.
func driveEnv(t *testing.T, stdin string) (*Env, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	p, err := paths.Resolve(paths.Options{
		Mode: paths.ModeUser,
		Root: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Env{
		Prompter: prompt.NewTestPrompter(strings.NewReader(stdin), out),
		Paths:    p,
		Config:   &config.LoadResult{},
		State:    &state.State{},
	}, out
}

// TestDispatch_QuitImmediately: typing "q" at the top-level menu
// exits the loop and returns nil.  Smoke test for the lifecycle.
func TestDispatch_QuitImmediately(t *testing.T) {
	env, out := driveEnv(t, "q\n")
	if err := Run(context.Background(), env); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "bye") {
		t.Errorf("missing goodbye in:\n%s", out.String())
	}
}

// TestDispatch_InspectEmptyRepo: pick #4 (inspect) with zero
// deployments configured — the flow should print the "no
// deployments" hint and return to the menu, where we quit.  This
// is the end-to-end test that proves the menu wiring (prompt →
// flow.Run → return → re-prompt → quit) actually round-trips.
func TestDispatch_InspectEmptyRepo(t *testing.T) {
	env, out := driveEnv(t, "4\nq\n")
	if err := Run(context.Background(), env); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "What would you like to do?") {
		t.Errorf("menu header missing:\n%s", s)
	}
	if !strings.Contains(s, "→ inspect repo") {
		t.Errorf("flow header missing:\n%s", s)
	}
	if !strings.Contains(s, "No deployments are configured") {
		t.Errorf(`expected "no deployments" message:\n%s`, s)
	}
	if !strings.Contains(s, "bye") {
		t.Errorf("did not exit cleanly:\n%s", s)
	}
}

// TestDispatch_InvalidThenValidChoice: typing a nonsense answer
// re-prompts the menu rather than killing the session.  Operators
// fat-finger; the loop should tolerate it.
func TestDispatch_InvalidThenValidChoice(t *testing.T) {
	env, out := driveEnv(t, "99\nq\n")
	if err := Run(context.Background(), env); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "pick 1..6") {
		t.Errorf("range hint missing:\n%s", s)
	}
}
