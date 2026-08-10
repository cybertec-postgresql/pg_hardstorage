package restore

// tablespace_path_test.go — coverage for the tablespace path-remap
// decision, which routes potentially-huge tablespace data to its
// restore location. parseTablespaceMapLinePath had no test, and
// tablespaceDestRoots silently keeps the SOURCE location when the
// parse returns "" — a silent misplacement if it were ever reachable.
// This pins the parser and proves the fallback is unreachable through
// the validated remap constructor (non-empty absolute New).

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

func TestParseTablespaceMapLinePath(t *testing.T) {
	cases := []struct{ line, want string }{
		{"16385 /mnt/space1\n", "/mnt/space1"},
		{"16385 /mnt/space1", "/mnt/space1"},           // no trailing newline
		{"16385 /mnt/my space/ts", "/mnt/my space/ts"}, // PATH CONTAINS SPACES — split on FIRST only
		{"1 /a", "/a"},
		{"", ""},          // empty
		{"16385", ""},     // oid only, no path
		{"16385 ", ""},    // trailing space, empty path
		{" /leading", ""}, // no oid (space at index 0)
	}
	for _, c := range cases {
		if got := parseTablespaceMapLinePath(c.line); got != c.want {
			t.Errorf("parseTablespaceMapLinePath(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}

func FuzzParseTablespaceMapLinePath(f *testing.F) {
	f.Add("16385 /mnt/space1\n")
	f.Add("16385 /mnt/my space")
	f.Add("")
	f.Add("garbage")
	f.Fuzz(func(t *testing.T, line string) {
		got := parseTablespaceMapLinePath(line) // must never panic
		// A non-empty result must be a real substring of the input
		// (the parser only slices, never fabricates).
		if got != "" && !containsSub(line, got) {
			t.Fatalf("parseTablespaceMapLinePath(%q) = %q, which is not a substring of the input",
				line, got)
		}
	})
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return len(sub) == 0
}

// TestTablespaceDestRoots_RemapNeverSilentlyFallsBackToSource: for any
// remap the validated constructor accepts, the destination for an
// external tablespace must be the REMAPPED path — never the source
// location. The silent p!="" fallback in tablespaceDestRoots is only
// safe because ParseTablespaceRemap forbids an empty/relative New;
// this pins that the fallback stays unreachable.
func TestTablespaceDestRoots_RemapNeverSilentlyFallsBackToSource(t *testing.T) {
	m := &backup.Manifest{
		Tablespaces: []backup.Tablespace{
			{OID: 16385, Location: "/srv/old ts/space1"}, // source path with a space
			{OID: 16386, Location: "/srv/old2"},
		},
	}
	remap, err := ParseTablespaceRemap([]string{
		"/srv/old ts/space1=/mnt/new ts/space1", // remap a spaced path to a spaced path
		"/srv/old2=/mnt/new2",
	})
	if err != nil {
		t.Fatalf("ParseTablespaceRemap: %v", err)
	}
	dests := tablespaceDestRoots(m, remap)
	for oid, want := range map[uint32]string{
		16385: "/mnt/new ts/space1",
		16386: "/mnt/new2",
	} {
		if dests[oid] != want {
			t.Errorf("tablespace %d dest = %q, want %q — a validated remap silently fell "+
				"back to (or mangled) the location; tablespace data would be written to the "+
				"wrong directory", oid, dests[oid], want)
		}
	}
}
