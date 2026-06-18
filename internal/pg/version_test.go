package pg_test

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in              string
		wantMajor       int
		wantMinor       int
		wantParseErr    bool
		wantContainsRaw string // must be in v.Raw
	}{
		{"17.2", 17, 2, false, "17.2"},
		{"17.2 (Debian 17.2-1.pgdg120+1)", 17, 2, false, "Debian"},
		{"16.5", 16, 5, false, ""},
		{"15.10 (Ubuntu 15.10-1.pgdg22.04+1)", 15, 10, false, "Ubuntu"},
		{"17", 17, 0, false, ""},
		{"17devel", 17, 0, false, "devel"},
		{"17beta1", 17, 0, false, "beta"},
		{"", 0, 0, true, ""},
		{"not a version", 0, 0, true, ""},
		{"v17.2", 0, 0, true, ""}, // "v" prefix isn't supported
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := pg.ParseVersion(c.in)
			if (err != nil) != c.wantParseErr {
				t.Fatalf("err=%v wantParseErr=%v", err, c.wantParseErr)
			}
			if c.wantParseErr {
				return
			}
			if got.Major != c.wantMajor || got.Minor != c.wantMinor {
				t.Errorf("major=%d minor=%d, want %d.%d", got.Major, got.Minor, c.wantMajor, c.wantMinor)
			}
			if got.Raw != c.in {
				t.Errorf("Raw = %q, want exact echo %q", got.Raw, c.in)
			}
		})
	}
}

func TestVersion_AtLeast(t *testing.T) {
	cases := []struct {
		v          pg.Version
		major      int
		minor      int
		wantAccept bool
	}{
		{pg.Version{Major: 17, Minor: 2}, 17, 2, true}, // equal
		{pg.Version{Major: 17, Minor: 2}, 17, 1, true},
		{pg.Version{Major: 17, Minor: 2}, 17, 3, false},
		{pg.Version{Major: 17, Minor: 0}, 16, 99, true},  // higher major
		{pg.Version{Major: 16, Minor: 99}, 17, 0, false}, // lower major
		{pg.Version{Major: 15, Minor: 5}, 15, 5, true},
	}
	for _, c := range cases {
		got := c.v.AtLeast(c.major, c.minor)
		if got != c.wantAccept {
			t.Errorf("Version{%d.%d}.AtLeast(%d, %d) = %v, want %v",
				c.v.Major, c.v.Minor, c.major, c.minor, got, c.wantAccept)
		}
	}
}

func TestVersion_String(t *testing.T) {
	v := pg.Version{Major: 17, Minor: 2, Raw: "17.2 (Debian 17.2-1.pgdg120+1)"}
	got := v.String()
	if got == "" {
		t.Error("String() must not be empty")
	}
	for _, sub := range []string{"PostgreSQL", "17.2", "Debian"} {
		if !contains(got, sub) {
			t.Errorf("String() missing %q: %s", sub, got)
		}
	}
}

func TestMode_String(t *testing.T) {
	for in, want := range map[pg.Mode]string{
		pg.ModeRegular:     "regular",
		pg.ModeReplication: "replication",
	} {
		if got := in.String(); got != want {
			t.Errorf("Mode(%d).String() = %q, want %q", in, got, want)
		}
	}
}

// TestSupportedMajors_CoversWindow: SupportedMajors returns
// every integer in [MinSupportedMajor, MaxSupportedMajor] in
// ascending order, with no gaps.
func TestSupportedMajors_CoversWindow(t *testing.T) {
	got := pg.SupportedMajors()
	want := pg.MaxSupportedMajor - pg.MinSupportedMajor + 1
	if len(got) != want {
		t.Fatalf("len(SupportedMajors) = %d, want %d", len(got), want)
	}
	for i, m := range got {
		expected := pg.MinSupportedMajor + i
		if m != expected {
			t.Errorf("SupportedMajors[%d] = %d, want %d (ascending, no gaps)", i, m, expected)
		}
	}
}

// TestPG18_InWindow: explicit assertion that PG 18 is in the
// support window. Bump MaxSupportedMajor when a new major
// stabilises; this test fails until the move.
func TestPG18_InWindow(t *testing.T) {
	if !pg.IsSupportedMajor(18) {
		t.Fatalf("PG 18 must be in the supported window; MaxSupportedMajor=%d", pg.MaxSupportedMajor)
	}
}

// TestIsSupportedMajor: bracket the window — values just below
// the floor and just above the ceiling are unsupported.
func TestIsSupportedMajor(t *testing.T) {
	cases := []struct {
		in   int
		want bool
	}{
		{pg.MinSupportedMajor - 1, false},
		{pg.MinSupportedMajor, true},
		{pg.MaxSupportedMajor, true},
		{pg.MaxSupportedMajor + 1, false},
		{0, false},
		{-5, false},
	}
	for _, c := range cases {
		if got := pg.IsSupportedMajor(c.in); got != c.want {
			t.Errorf("IsSupportedMajor(%d) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestDefaultSandboxMajor_IsCurrent: the default sandbox
// major must equal MaxSupportedMajor — a bumped supported
// ceiling without bumping the default would silently keep
// pulling old images for new manifests.
func TestDefaultSandboxMajor_IsCurrent(t *testing.T) {
	if pg.DefaultSandboxMajor != pg.MaxSupportedMajor {
		t.Errorf("DefaultSandboxMajor = %d, want MaxSupportedMajor = %d (must track when a new major stabilises)",
			pg.DefaultSandboxMajor, pg.MaxSupportedMajor)
	}
}

// TestIncrementalMinMajor_PG17: regression — the incremental
// floor is PG 17. Pre-PG-17 manifests can't be incremental
// chain anchors and the runner refuses up-front.
func TestIncrementalMinMajor_PG17(t *testing.T) {
	if pg.IncrementalMinMajor != 17 {
		t.Errorf("IncrementalMinMajor = %d, want 17", pg.IncrementalMinMajor)
	}
	if pg.CombineBackupMinMajor != 17 {
		t.Errorf("CombineBackupMinMajor = %d, want 17", pg.CombineBackupMinMajor)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
