package cli

// The --remove-target / --remove-targets pair: singular on the
// one-session commands, plural on the reap-everything command — honest
// names individually, a usage error at 3am for anyone scripting both.
// Both spellings must work on all three commands, while --help keeps
// each command's own name.

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestRemoveTargetSpellings_Interchangeable(t *testing.T) {
	for _, tc := range []struct {
		build    func() interface{ ParseFlags([]string) error }
		declared string
		other    string
	}{
		{func() interface{ ParseFlags([]string) error } { return newStandbyDestroyCmd() }, "remove-target", "--remove-targets"},
		{func() interface{ ParseFlags([]string) error } { return newTimeTravelDestroyCmd() }, "remove-target", "--remove-targets"},
		{func() interface{ ParseFlags([]string) error } { return newTimeTravelCleanupCmd() }, "remove-targets", "--remove-target"},
	} {
		c := tc.build()
		if err := c.ParseFlags([]string{tc.other}); err != nil {
			t.Errorf("%T: the sibling spelling %s was rejected: %v", c, tc.other, err)
		}
	}
}

func TestPGConnectionSpelling_WorksOnDeploymentCommands(t *testing.T) {
	// Twenty-odd commands say --pg-connection; deployment add/edit
	// alone said --connection. Both must parse on both.
	for _, build := range []func() *cobra.Command{newDeploymentAddCmd, newDeploymentEditCmd} {
		c := build()
		if err := c.ParseFlags([]string{"--pg-connection", "host=x"}); err != nil {
			t.Errorf("%s: --pg-connection rejected: %v", c.Name(), err)
		}
		if f := c.Flags().Lookup("connection"); f == nil || f.Value.String() != "host=x" {
			t.Errorf("%s: --pg-connection did not land on --connection", c.Name())
		}
	}
}
