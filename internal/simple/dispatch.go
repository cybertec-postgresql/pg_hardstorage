// dispatch.go — top-level menu (setup/backup/stream/inspect/verify/…) for pg_hardstorage_simple.
package simple

import (
	"context"
	"errors"
	"fmt"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/simple/prompt"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/simple/state"
)

// menuChoices is the fixed top-level menu.  Order matches the
// design doc's "ranked by Day-0 timeline": setup first, take-a-
// backup second, then the rest.  Indexes are stable across releases
// (we add to the end, never reorder) so an operator's muscle memory
// of "2 is backup, 3 is stream" survives upgrades.
var menuChoices = []struct {
	Label string
	Flow  Flow
}{
	{Label: "Set up backups for a database I haven't backed up before", Flow: &flowSetup{}},
	{Label: "Take a backup right now", Flow: &flowBackup{}},
	{Label: "Start continuous protection (base backup + WAL streaming)", Flow: &flowStream{}},
	{Label: "See what's in my repository", Flow: &flowInspect{}},
	{Label: "Verify a backup is restorable", Flow: &flowVerify{}},
	{Label: "Restore a backup", Flow: &flowRestore{}},
}

// Header is printed once when the binary starts (and reprinted by
// the dispatch loop on every iteration so a long-running flow's
// output doesn't push it off-screen).  Kept short — this is the
// whole "branding" surface.
const Header = "\n  pg_hardstorage — quick start\n"

// Run is the main loop.  Prints the menu, dispatches the picked
// flow, saves State on success, loops back.  Returns nil on operator
// quit (prompt.ErrQuit), surfaces any other error to the caller.
func Run(ctx context.Context, env *Env) error {
	for {
		env.Prompter.Println(Header)
		choices := make([]prompt.Choice, len(menuChoices))
		for i, m := range menuChoices {
			choices[i] = prompt.Choice{Label: m.Label}
		}
		idx, err := env.Prompter.PromptChoice("What would you like to do?", choices, -1)
		if errors.Is(err, prompt.ErrQuit) {
			env.Prompter.Println("  bye.\n")
			return nil
		}
		if err != nil {
			return err
		}
		flow := menuChoices[idx].Flow
		env.Prompter.Printf("\n  → %s\n\n", flow.Name())
		if err := flow.Run(ctx, env); err != nil {
			if errors.Is(err, prompt.ErrQuit) {
				env.Prompter.Println("  bye.\n")
				return nil
			}
			// Flow-level failures print and continue — operators
			// often want to retry from the menu rather than have
			// the binary exit with a non-zero code.
			env.Prompter.Printf("  ✗ %s failed: %v\n\n", flow.Name(), err)
			continue
		}
		// Persist state on success.  Saving on failure too would
		// leave half-set defaults behind ("LastRepoURL = the one
		// that failed validation") — we'd rather re-ask.
		if err := saveStateBestEffort(env); err != nil {
			env.Prompter.Printf("  (warn: could not save preferences: %v)\n", err)
		}
		env.Prompter.Println("")
	}
}

// saveStateBestEffort writes State to <Config>/simple.yaml.  The
// "best effort" part: a write failure prints a warning but doesn't
// abort the session — the operator's just lost the convenience of
// "remember last-picked", not anything load-bearing.
func saveStateBestEffort(env *Env) error {
	if env.Paths == nil || env.Paths.Config.Value == "" {
		return fmt.Errorf("simple state: empty Config path")
	}
	return state.Save(env.Paths.Config.Value, env.State)
}
