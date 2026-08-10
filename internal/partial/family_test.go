package partial

// family_test.go — isFamilyMember decides which files get pulled into
// a single-table restore. It had no direct test, and its failure mode
// is the worst in the system: a false match grabs a NEIGHBORING
// table's file, so "restore accounts" silently writes audit_log's
// bytes. The specific trap is PG's numeric relfilenode naming — 1259
// is a string-prefix of 12590 — so a naive prefix check leaks across
// tables. This pins that it does not.

import "testing"

func TestIsFamilyMember(t *testing.T) {
	const base = "base/16384/1259"
	cases := []struct {
		path string
		want bool
		why  string
	}{
		// --- Real family members (must match).
		{base, true, "the base heap file itself"},
		{base + ".1", true, "segment 1"},
		{base + ".42", true, "segment 42"},
		{base + "_vm", true, "visibility map fork"},
		{base + "_fsm", true, "free-space map fork"},
		{base + "_init", true, "init fork (unlogged)"},
		{base + "_vm.1", true, "vm fork, segment 1"},
		{base + "_fsm.3", true, "fsm fork, segment 3"},

		// --- The prefix-collision trap: a DIFFERENT relfilenode that
		// happens to share a numeric prefix. Every one of these is
		// another table's file and MUST NOT be grabbed.
		{"base/16384/12590", false, "12590 is a different table, not a segment of 1259"},
		{"base/16384/12591", false, "12591 is a different table"},
		{"base/16384/125", false, "125 is a shorter different table (not even a prefix match)"},
		{"base/16384/12590_vm", false, "12590's vm fork is 12590's, not 1259's"},
		{"base/16384/12590.1", false, "12590's segment is not 1259's family"},

		// --- Malformed suffixes (must not match).
		{base + "x", false, "trailing junk"},
		{base + ".", false, "dot with no segment number"},
		{base + ".1a", false, "non-numeric segment"},
		{base + "_vmx", false, "_vmx is not a real fork"},
		{base + "_", false, "bare underscore"},

		// --- Different database directory (must not match).
		{"base/99999/1259", false, "same relfilenode, different database oid"},
	}
	for _, c := range cases {
		if got := isFamilyMember(c.path, base); got != c.want {
			t.Errorf("isFamilyMember(%q, %q) = %v, want %v — %s", c.path, base, got, c.want, c.why)
		}
	}
}

// FuzzIsFamilyMember — no input pair panics, and the anti-collision
// invariant holds structurally: if the path matches, the character
// right after the base prefix is '.', '_', or the strings are equal —
// never a bare digit (which would be a neighboring relfilenode).
func FuzzIsFamilyMember(f *testing.F) {
	f.Add("base/16384/1259", "base/16384/1259")
	f.Add("base/16384/12590", "base/16384/1259")
	f.Add("base/16384/1259_vm.7", "base/16384/1259")
	f.Fuzz(func(t *testing.T, path, base string) {
		if !isFamilyMember(path, base) {
			return
		}
		if path == base {
			return
		}
		// A true match on a longer path: the first differing byte must
		// start a fork tag or a segment dot — NOT a digit continuing a
		// relfilenode number.
		if len(path) <= len(base) {
			t.Fatalf("matched %q against longer base %q", path, base)
		}
		next := path[len(base)]
		if next >= '0' && next <= '9' {
			t.Fatalf("isFamilyMember(%q, %q)=true but the byte after the base is a DIGIT (%q) "+
				"— this is the prefix-collision leak: a different relfilenode was pulled into "+
				"the family", path, base, string(next))
		}
	})
}
