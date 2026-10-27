package restore

import (
	"path/filepath"
	"strings"
	"testing"
)

// Whatever a (hostile) manifest puts in a path, safeJoinTarget either
// refuses it or returns a path strictly inside target.
func FuzzSafeJoinTarget(f *testing.F) {
	for _, s := range []string{"base/1/2", "../x", "/etc/passwd", "a/../../b", "a/./b", "..", ".", "a//b", "a/..\\..", "\x00", "a/b/../../..", "....//x"} {
		f.Add(s)
	}
	target := f.TempDir()
	f.Fuzz(func(t *testing.T, rel string) {
		got, err := safeJoinTarget(target, rel)
		if err != nil {
			return
		}
		r, rerr := filepath.Rel(target, got)
		if rerr != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r) {
			t.Fatalf("safeJoinTarget(%q) = %q escapes %q", rel, got, target)
		}
	})
}
