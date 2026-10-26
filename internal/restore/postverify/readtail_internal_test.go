package postverify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadTail_OnlyTheTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(p, []byte(strings.Repeat("a", 100000)+"TAIL"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readTail(p, 10); got != "aaaaaaTAIL" {
		t.Fatalf("got %q", got)
	}
	if got := readTail(p, 1<<20); len(got) != 100004 {
		t.Fatalf("short file: len %d", len(got))
	}
	if got := readTail(filepath.Join(t.TempDir(), "missing"), 10); got != "" {
		t.Fatalf("missing: %q", got)
	}
}
