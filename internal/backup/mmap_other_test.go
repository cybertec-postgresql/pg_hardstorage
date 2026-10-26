//go:build !unix

package backup

import (
	"os"
	"path/filepath"
	"testing"
)

// On platforms without mmap, mapReadOnly must refuse above the cap
// instead of allocating the whole manifest.
func TestMapReadOnly_NonUnixRefusesOverCap(t *testing.T) {
	defer func(n int64) { maxHeldSignBytes = n }(maxHeldSignBytes)
	maxHeldSignBytes = 4
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("abcdefgh"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(p)
	defer f.Close()
	if _, _, err := mapReadOnly(f, 8); err == nil {
		t.Fatal("want error over the cap")
	}
	if b, _, err := mapReadOnly(f, 4); err != nil || string(b) != "abcd" {
		t.Fatalf("under cap: %q %v", b, err)
	}
}
