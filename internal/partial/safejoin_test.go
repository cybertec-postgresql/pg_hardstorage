package partial

import (
	"path/filepath"
	"strings"
	"testing"
)

// Mirror of restore's safe-join tests.  partial keeps an independent
// copy of the helper on purpose; we keep the test coverage independent
// too.

func TestSafeJoinTarget_HappyPaths(t *testing.T) {
	target := t.TempDir()
	cases := []struct {
		name string
		rel  string
		want string
	}{
		{"simple file", "PG_VERSION", filepath.Join(target, "PG_VERSION")},
		{"nested", "base/16384/2619", filepath.Join(target, "base/16384/2619")},
		{"dot-segment cancels", "base/./16384/2619", filepath.Join(target, "base/16384/2619")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := safeJoinTarget(target, tc.rel)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSafeJoinTarget_EscapesRefused(t *testing.T) {
	target := t.TempDir()
	cases := []struct {
		name string
		rel  string
	}{
		{"single dot-dot", ".."},
		{"dot-dot prefix", "../etc/passwd"},
		{"interleaved escape", "base/../../etc/passwd"},
		{"absolute posix", "/etc/passwd"},
		{"empty rel", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := safeJoinTarget(target, tc.rel)
			if err == nil {
				t.Fatalf("expected refusal for %q", tc.rel)
			}
			if !strings.Contains(err.Error(), "partial:") {
				t.Errorf("error not namespaced: %q", err.Error())
			}
		})
	}
}
