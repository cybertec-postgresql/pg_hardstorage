package cli_test

import (
	"strings"
	"testing"
)

// `logical add` derives a stream's slot (pg_hardstorage_logical_<name>)
// when --slot is not given. PostgreSQL allows only lower-case letters,
// digits and '_' in slot names, so the raw name of a stream like
// my-stream or Orders produced a slot no `logical stream` run could ever
// create. The default is now normalised (lower-case, '-' and '.' → '_');
// a derived slot that is still invalid (too long, other characters) is
// refused at add time instead of failing every later stream.
func TestLogicalAdd_DerivedSlotIsNormalisedAndValidated(t *testing.T) {
	newReadWorld(t)
	for _, name := range []string{"my-stream", "Orders", "l3.sub-1"} {
		out, errb, exit := runCmd(t, "logical", "add", name,
			"--deployment", "db1", "--repo", "file:///tmp/r", "--publication", "pub", "-o", "json")
		if exit != 0 {
			t.Errorf("logical add %q: exit %d, want 0 (normalised slot)\n%s", name, exit, errb)
			continue
		}
		want := "pg_hardstorage_logical_" + strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToLower(name))
		if !strings.Contains(out, want) {
			t.Errorf("logical add %q: slot %q not in result\n%s", name, want, out)
		}
	}
	for _, name := range []string{strings.Repeat("x", 50), "bad name"} {
		_, errb, exit := runCmd(t, "logical", "add", name,
			"--deployment", "db1", "--repo", "file:///tmp/r", "--publication", "pub", "-o", "json")
		if exit != 2 || !strings.Contains(errb, "usage.bad_slot") {
			t.Errorf("logical add %q: exit %d, want 2 with usage.bad_slot\n%s", name, exit, errb)
		}
	}
	// An explicit --slot is used as given and must itself be valid.
	if _, errb, exit := runCmd(t, "logical", "add", "other", "--slot", "Bad-Slot",
		"--deployment", "db1", "--repo", "file:///tmp/r", "--publication", "pub", "-o", "json"); exit != 2 || !strings.Contains(errb, "usage.bad_slot") {
		t.Errorf("logical add --slot Bad-Slot: exit %d, want 2\n%s", exit, errb)
	}
}
