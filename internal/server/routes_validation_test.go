package server

import (
	"strings"
	"testing"
)

// ----- parseLimit -----

func TestParseLimit_DefaultsWhenEmpty(t *testing.T) {
	got, err := parseLimit("", 200, 10000)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != 200 {
		t.Errorf("got %d, want default 200", got)
	}
}

func TestParseLimit_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"1", 1},
		{"100", 100},
		{"10000", 10000},
	} {
		got, err := parseLimit(tc.raw, 200, 10000)
		if err != nil {
			t.Errorf("%q err = %v", tc.raw, err)
		}
		if got != tc.want {
			t.Errorf("%q got %d, want %d", tc.raw, got, tc.want)
		}
	}
}

func TestParseLimit_ClampsAboveMax(t *testing.T) {
	got, err := parseLimit("1000000", 200, 10000)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != 10000 {
		t.Errorf("expected clamp to max 10000, got %d", got)
	}
}

func TestParseLimit_ZeroDefaults(t *testing.T) {
	// Explicit "0" is the same as "absent" for our purposes — apply
	// the default rather than returning the no-LIMIT-clause sentinel.
	got, err := parseLimit("0", 200, 10000)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != 200 {
		t.Errorf("got %d, want default 200 for zero", got)
	}
}

func TestParseLimit_NegativeRejected(t *testing.T) {
	for _, tc := range []string{"-1", "-100", "-10000"} {
		_, err := parseLimit(tc, 200, 10000)
		if err == nil {
			t.Errorf("%q: expected error", tc)
		}
		if !strings.Contains(err.Error(), "≥ 0") {
			t.Errorf("%q: error %q doesn't name the cause", tc, err.Error())
		}
	}
}

func TestParseLimit_MalformedRejected(t *testing.T) {
	for _, tc := range []string{
		"abc", "1.5", "1e5", "0x10", "1 2", "100; DROP TABLE",
	} {
		_, err := parseLimit(tc, 200, 10000)
		if err == nil {
			t.Errorf("%q: expected error for malformed input", tc)
		}
	}
}

// ----- validateRestoreTargetDir -----

func TestValidateRestoreTargetDir_RequiresAbsolute(t *testing.T) {
	for _, tc := range []string{"relative/path", "../escape", "data"} {
		err := validateRestoreTargetDir(tc, nil)
		if err == nil {
			t.Errorf("%q should be rejected (not absolute)", tc)
		}
	}
}

func TestValidateRestoreTargetDir_RejectsEmpty(t *testing.T) {
	if err := validateRestoreTargetDir("", nil); err == nil {
		t.Errorf("empty target_dir should be rejected")
	}
}

func TestValidateRestoreTargetDir_RejectsTraversal(t *testing.T) {
	for _, tc := range []string{
		"/var/lib/../etc/passwd",
		"/var/lib/./postgres",
		"/var//lib/postgres",
	} {
		err := validateRestoreTargetDir(tc, nil)
		if err == nil {
			t.Errorf("%q should be rejected (not normalised)", tc)
		}
	}
}

func TestValidateRestoreTargetDir_HappyNoRoots(t *testing.T) {
	for _, tc := range []string{
		"/var/lib/postgresql/restored",
		"/tmp/restore",
		"/srv/data/db1",
	} {
		err := validateRestoreTargetDir(tc, nil)
		if err != nil {
			t.Errorf("%q rejected unexpectedly: %v", tc, err)
		}
	}
}

func TestValidateRestoreTargetDir_AllowedRoots_Accepts(t *testing.T) {
	roots := []string{"/var/lib/postgresql", "/srv/restore"}
	for _, tc := range []string{
		"/var/lib/postgresql/db1",
		"/var/lib/postgresql/db1/data",
		"/srv/restore/x",
	} {
		err := validateRestoreTargetDir(tc, roots)
		if err != nil {
			t.Errorf("%q under allowed roots %v rejected: %v", tc, roots, err)
		}
	}
}

func TestValidateRestoreTargetDir_AllowedRoots_RejectsOutside(t *testing.T) {
	roots := []string{"/var/lib/postgresql"}
	for _, tc := range []string{
		"/etc/passwd",
		"/usr/local/bin",
		"/var/lib/other",
	} {
		err := validateRestoreTargetDir(tc, roots)
		if err == nil {
			t.Errorf("%q outside roots %v should be rejected", tc, roots)
		}
	}
}

// TestValidateRestoreTargetDir_AllowedRoots_RejectsParentEscape covers
// the subtle case where the requested path is a *prefix* of an
// allowed root (e.g. /var when /var/lib/postgresql is allowed).
// filepath.Rel returns ".." for that case; we must reject.
func TestValidateRestoreTargetDir_AllowedRoots_RejectsParentEscape(t *testing.T) {
	roots := []string{"/var/lib/postgresql"}
	if err := validateRestoreTargetDir("/var/lib", roots); err == nil {
		t.Errorf("/var/lib should be rejected — it's a parent of the allowed root")
	}
	if err := validateRestoreTargetDir("/var", roots); err == nil {
		t.Errorf("/var should be rejected")
	}
}
