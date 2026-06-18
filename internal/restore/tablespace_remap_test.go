package restore_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// TestParseTablespaceRemap_HappyPath: well-formed entries
// parse into a non-empty TablespaceRemap.
func TestParseTablespaceRemap_HappyPath(t *testing.T) {
	got, err := restore.ParseTablespaceRemap([]string{
		"/mnt/ssd/ts_fast=/var/lib/pg/ts_fast",
		"/mnt/hdd/ts_archive=/var/lib/pg/ts_archive",
	})
	if err != nil {
		t.Fatalf("ParseTablespaceRemap: %v", err)
	}
	if got.Empty() {
		t.Fatalf("expected non-empty remap")
	}
	if len(got) != 2 {
		t.Errorf("got %d entries, want 2", len(got))
	}
	if got[0].Old != "/mnt/ssd/ts_fast" || got[0].New != "/var/lib/pg/ts_fast" {
		t.Errorf("entry[0] = %+v", got[0])
	}
}

// TestParseTablespaceRemap_EmptyInput_NilResult: nil/empty
// input is the no-remap signal; the caller can use Apply
// unconditionally.
func TestParseTablespaceRemap_EmptyInput_NilResult(t *testing.T) {
	for _, in := range [][]string{nil, {}} {
		got, err := restore.ParseTablespaceRemap(in)
		if err != nil {
			t.Errorf("empty input should not error; got %v", err)
		}
		if !got.Empty() {
			t.Errorf("expected empty remap; got %v", got)
		}
	}
}

// TestParseTablespaceRemap_ValidationGuards: malformed entries
// refuse with operator-friendly messages.
func TestParseTablespaceRemap_ValidationGuards(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		errSubs string
	}{
		{"missing-equals", "/mnt/ssd/ts_fast", "OLD=NEW"},
		{"empty-old", "=/var/lib/pg/ts_fast", "OLD=NEW"},
		{"empty-new", "/mnt/ssd/ts_fast=", "OLD=NEW"},
		{"relative-old", "ts_fast=/var/lib/pg/ts_fast", "must be absolute"},
		{"relative-new", "/mnt/ssd/ts_fast=ts_fast", "must be absolute"},
		{"empty-entry", "", "is empty"},
		// Control characters break the tablespace_map "<oid> <path>"
		// line format. A newline in NEW would forge a second OID→path
		// entry that PG turns into a symlink (tablespace-entry
		// injection); NUL is never a valid path byte. Reject at the
		// parser — the single chokepoint the CLI and the control-plane
		// agent both route through.
		{"newline-in-new", "/old/ts=/new/ts\n99999 /attacker/path", "control character"},
		{"newline-in-old", "/a\nb=/new/ts", "control character"},
		{"cr-in-new", "/old/ts=/new\r/ts", "control character"},
		{"nul-in-new", "/old/ts=/new\x00ts", "control character"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := restore.ParseTablespaceRemap([]string{c.in})
			if err == nil {
				t.Fatalf("expected error for %q", c.in)
			}
			if !strings.Contains(err.Error(), c.errSubs) {
				t.Errorf("err = %q; want substring %q", err, c.errSubs)
			}
		})
	}
}

// TestParseTablespaceRemap_DuplicateOld_Refused: duplicate
// OLD path refuses (last-wins would silently mask a typo).
func TestParseTablespaceRemap_DuplicateOld_Refused(t *testing.T) {
	_, err := restore.ParseTablespaceRemap([]string{
		"/a=/b",
		"/a=/c",
	})
	if err == nil {
		t.Fatal("duplicate OLD path should refuse")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("err = %v; want 'duplicate'", err)
	}
}

// TestApply_RewritesMatchingPaths: a tablespace_map body with
// two entries gets one path rewritten and the other untouched.
// Trailing newline preserved.
func TestApply_RewritesMatchingPaths(t *testing.T) {
	body := "1663 /mnt/ssd/ts_fast\n1664 /mnt/hdd/ts_archive\n"
	r, err := restore.ParseTablespaceRemap([]string{
		"/mnt/ssd/ts_fast=/var/lib/pg/ts_fast",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := r.Apply(body)
	want := "1663 /var/lib/pg/ts_fast\n1664 /mnt/hdd/ts_archive\n"
	if got != want {
		t.Errorf("Apply = %q\nwant %q", got, want)
	}
}

// TestApply_PreservesOIDs: the OID column is left alone even
// when its path matches an OLD entry.
func TestApply_PreservesOIDs(t *testing.T) {
	body := "12345 /old/path\n"
	r, _ := restore.ParseTablespaceRemap([]string{"/old/path=/new/path"})
	got := r.Apply(body)
	if !strings.HasPrefix(got, "12345 ") {
		t.Errorf("OID should be preserved; got %q", got)
	}
}

// TestApply_MultipleMatchingEntries: every matching OLD path
// across multiple lines is rewritten.
func TestApply_MultipleMatchingEntries(t *testing.T) {
	body := "1 /a\n2 /b\n3 /c\n"
	r, _ := restore.ParseTablespaceRemap([]string{
		"/a=/A",
		"/b=/B",
	})
	got := r.Apply(body)
	want := "1 /A\n2 /B\n3 /c\n"
	if got != want {
		t.Errorf("Apply = %q\nwant %q", got, want)
	}
}

// TestApply_EmptyRemap_NoOp: empty receiver returns the body
// verbatim — callers can use Apply unconditionally.
func TestApply_EmptyRemap_NoOp(t *testing.T) {
	body := "1663 /mnt/ssd/ts_fast\n"
	var r restore.TablespaceRemap
	if got := r.Apply(body); got != body {
		t.Errorf("empty remap should no-op; got %q", got)
	}
}

// TestApply_EmptyBody_NoOp: empty body input is returned
// verbatim regardless of remap.
func TestApply_EmptyBody_NoOp(t *testing.T) {
	r, _ := restore.ParseTablespaceRemap([]string{"/a=/b"})
	if got := r.Apply(""); got != "" {
		t.Errorf("empty body should no-op; got %q", got)
	}
}

// TestApply_GarbageLines_PassedThrough: malformed lines (no
// space, comments, empty) are passed through verbatim. PG
// would reject these at recovery time either way.
func TestApply_GarbageLines_PassedThrough(t *testing.T) {
	body := "1663 /a\n# a comment\n\nbroken-no-space\n1664 /b\n"
	r, _ := restore.ParseTablespaceRemap([]string{"/a=/A", "/b=/B"})
	got := r.Apply(body)
	want := "1663 /A\n# a comment\n\nbroken-no-space\n1664 /B\n"
	if got != want {
		t.Errorf("garbage should pass through; got %q\nwant %q", got, want)
	}
}

// TestApply_PathWithSpaces: PG's tablespace_map splits on the
// first space; paths with embedded spaces are valid. Apply
// must rewrite them as a unit.
func TestApply_PathWithSpaces(t *testing.T) {
	body := "1663 /mnt/with space/ts\n"
	r, _ := restore.ParseTablespaceRemap([]string{"/mnt/with space/ts=/var/lib/pg/ts"})
	got := r.Apply(body)
	want := "1663 /var/lib/pg/ts\n"
	if got != want {
		t.Errorf("Apply with spaces = %q\nwant %q", got, want)
	}
}

// TestToCombineArgs_FlagShape: --tablespace-mapping=OLD=NEW
// per entry, in receiver order.
func TestToCombineArgs_FlagShape(t *testing.T) {
	r, _ := restore.ParseTablespaceRemap([]string{
		"/mnt/a=/var/A",
		"/mnt/b=/var/B",
	})
	got := r.ToCombineArgs()
	want := []string{
		"--tablespace-mapping=/mnt/a=/var/A",
		"--tablespace-mapping=/mnt/b=/var/B",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d args, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestToCombineArgs_EmptyRemap_NilSlice: callers can append
// the result unconditionally without nil-checking.
func TestToCombineArgs_EmptyRemap_NilSlice(t *testing.T) {
	var r restore.TablespaceRemap
	got := r.ToCombineArgs()
	if len(got) != 0 {
		t.Errorf("empty remap should yield no args; got %v", got)
	}
}

// TestAppliedPaths_OrderPreserved: the New paths come back
// in receiver order so the result body's surface is stable.
func TestAppliedPaths_OrderPreserved(t *testing.T) {
	r, _ := restore.ParseTablespaceRemap([]string{
		"/a=/A",
		"/b=/B",
		"/c=/C",
	})
	got := r.AppliedPaths()
	want := []string{"/A", "/B", "/C"}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
