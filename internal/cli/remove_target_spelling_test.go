package cli

// The --remove-target / --remove-targets pair: singular on the
// one-session commands, plural on the reap-everything command — honest
// names individually, a usage error at 3am for anyone scripting both.
// Both spellings must work on all three commands, while --help keeps
// each command's own name.

import (
	"testing"
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
