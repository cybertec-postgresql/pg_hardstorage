package cli

import (
	"strconv"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
)

// Found by ultrareview: renderHotCommandHelp is documented to STOP at
// the first entry that overflows its byte budget, because the list is
// priority-ordered. It used `continue` instead, so a large
// high-priority command could be dropped while a smaller,
// lower-priority one behind it made the cut — the block's contents
// followed help-text length, not importance.
//
// Property: for any budget, the rendered entries are a PREFIX of
// hotCommandPaths (ignoring entries with no help at all).
func TestHotCommandHelpIsAPriorityPrefix(t *testing.T) {
	tree := cmdtree.Walk(NewRoot())
	var helps []string
	for _, p := range hotCommandPaths {
		if h := cmdtree.Help(tree, p); h != "" {
			helps = append(helps, h)
		}
	}
	if len(helps) < 3 {
		t.Fatalf("too few hot commands with help (%d) for this test to mean anything", len(helps))
	}
	// Construct the budgets that expose the bug: for entry i, choose a
	// budget where i does NOT fit but some later, smaller entry j would.
	// A sweep over round numbers can miss every such window.
	prefix := 0
	cases := 0
	for i := range helps {
		for j := i + 1; j < len(helps); j++ {
			if len(helps[j]) >= len(helps[i]) {
				continue
			}
			budget := prefix + len(helps[j]) + 1 // j fits after the prefix; i does not
			t.Setenv("PG_HARDSTORAGE_LLM_HOT_HELP_BYTES", strconv.Itoa(budget))
			out := renderHotCommandHelp(tree)
			cases++
			if strings.Contains(out, helps[j]) && !strings.Contains(out, helps[i]) {
				t.Errorf("budget %d: lower-priority entry %d rendered after higher-priority entry %d was dropped", budget, j, i)
			}
			break
		}
		prefix += len(helps[i]) + 1
	}
	if cases == 0 {
		t.Skip("no hot command is followed by a smaller one; nothing to exercise")
	}
}
