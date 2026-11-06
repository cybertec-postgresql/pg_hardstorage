package spill

import (
	"errors"
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
	_, _, err = readFileBounded(f, 10, 9)
	if !errors.Is(err, ErrTooLargeToHold) || !strings.Contains(err.Error(), "mmap") {
		t.Fatalf("over the limit: want a clear mmap error wrapping ErrTooLargeToHold, got %v", err)
	}
}

// MapReadOnly hands back exactly the file's bytes on every platform (a
// mapping on unix, a heap copy elsewhere), and an empty file is not an
// error.
func TestMapReadOnly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, release, err := MapReadOnly(f, 10, 1<<20)
	if err != nil || string(b) != "0123456789" {
		t.Fatalf("MapReadOnly: %q %v", b, err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	b, release, err = MapReadOnly(f, 0, 1<<20)
	if err != nil || len(b) != 0 {
		t.Fatalf("empty: %q %v", b, err)
	}
	if err := release(); err != nil {
		t.Fatalf("release empty: %v", err)
	}
}
