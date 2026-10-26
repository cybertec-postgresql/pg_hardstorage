package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFileBounded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, done, err := readFileBounded(f, 10, 10)
	if err != nil || string(b) != "0123456789" {
		t.Fatalf("at the limit: %q %v", b, err)
	}
	_ = done()
	if _, _, err := readFileBounded(f, 10, 9); err == nil || !strings.Contains(err.Error(), "mmap") {
		t.Fatalf("over the limit: want a clear mmap error, got %v", err)
	}
}
