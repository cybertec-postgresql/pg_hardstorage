package cli_test

import (
	"regexp"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
)

// Regression (LOW): `recovery drill history --help` told operators to
// suppress history with --skip-history; the drill's real flag is
// --no-history, so following the help failed with "unknown flag".
// Every --flag the help mentions must exist on the history command or
// on `recovery drill` (whose runs it describes).
func TestRecoveryDrillHistory_HelpNamesRealFlags(t *testing.T) {
	root := cli.NewRoot()
	hist, _, err := root.Find([]string{"recovery", "drill", "history"})
	if err != nil {
		t.Fatal(err)
	}
	drill, _, err := root.Find([]string{"recovery", "drill"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`--([a-z][a-z-]+)`).FindAllStringSubmatch(hist.Long, -1) {
		name := m[1]
		if hist.Flags().Lookup(name) == nil && hist.InheritedFlags().Lookup(name) == nil &&
			drill.Flags().Lookup(name) == nil && root.PersistentFlags().Lookup(name) == nil {
			t.Errorf("help mentions --%s, which neither `recovery drill history` nor `recovery drill` has", name)
		}
	}
}
