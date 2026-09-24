package cli_test

import (
	"strings"
	"testing"
)

// `logical add` validated an explicit --slot but not the default slot it
// derives from the stream name (pg_hardstorage_logical_<name>), so a
// stream called my-stream registered fine and every later `logical
// stream` failed on an invalid replication-slot identifier.
func TestLogicalAdd_DerivedSlotIsValidated(t *testing.T) {
	newReadWorld(t)
	for _, name := range []string{"my-stream", "Orders", strings.Repeat("x", 50)} {
		_, errb, exit := runCmd(t, "logical", "add", name,
			"--deployment", "db1", "--repo", "file:///tmp/r", "--publication", "pub", "-o", "json")
		if exit != 2 || !strings.Contains(errb, "usage.bad_slot") {
			t.Errorf("logical add %q: exit %d, want 2 with usage.bad_slot\n%s", name, exit, errb)
		}
	}
	// A valid name, or an invalid one with a valid explicit --slot, is fine.
	if _, errb, exit := runCmd(t, "logical", "add", "orders",
		"--deployment", "db1", "--repo", "file:///tmp/r", "--publication", "pub", "-o", "json"); exit != 0 {
		t.Errorf("logical add orders: exit %d\n%s", exit, errb)
	}
	if _, errb, exit := runCmd(t, "logical", "add", "my-stream", "--slot", "my_stream",
		"--deployment", "db1", "--repo", "file:///tmp/r", "--publication", "pub", "-o", "json"); exit != 0 {
		t.Errorf("logical add my-stream --slot my_stream: exit %d\n%s", exit, errb)
	}
}
