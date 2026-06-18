package restore

import (
	"path/filepath"
	"strings"
	"testing"
)

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
		{"interior parent", "base/x/../16384/2619", filepath.Join(target, "base/16384/2619")},
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
		{"deep escape", "../../../../etc/shadow"},
		{"interleaved escape", "base/../../etc/passwd"},
		{"absolute posix", "/etc/passwd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := safeJoinTarget(target, tc.rel)
			if err == nil {
				t.Fatalf("expected refusal for %q", tc.rel)
			}
			// Match the human-readable signal: either "absolute" or
			// "escapes" must appear so operators see the cause.
			msg := err.Error()
			if !strings.Contains(msg, "absolute") && !strings.Contains(msg, "escapes") {
				t.Errorf("error %q doesn't name the cause", msg)
			}
		})
	}
}

func TestSafeJoinTarget_EmptyRefused(t *testing.T) {
	target := t.TempDir()
	if _, err := safeJoinTarget(target, ""); err == nil {
		t.Errorf("expected refusal for empty rel")
	}
}
