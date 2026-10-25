package recovery

import (
	"os"
	"path/filepath"
	"testing"
)

// clearDirContents empties an operator-owned directory the drill only
// borrowed, but must leave the directory itself in place.
func TestClearDirContents_KeepsTheDirectory(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"a", "sub/b", "sub/deeper/c"} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := clearDirContents(dir); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("directory itself was removed: %v", err)
	}
	if len(ents) != 0 {
		t.Fatalf("%d entries left", len(ents))
	}
	if err := clearDirContents(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing directory must be an error")
	}
}
