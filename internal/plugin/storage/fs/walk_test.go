package fs

import (
	"fmt"
	stdfs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// A directory larger than walkDirBatch is read in batches, and the walk
// still visits every file exactly once, subdirectories included.
func TestWalkDirBounded_BatchesLargeDirectories(t *testing.T) {
	old := walkDirBatch
	walkDirBatch = 3
	t.Cleanup(func() { walkDirBatch = old })

	root := t.TempDir()
	want := map[string]bool{}
	for i := 0; i < 10; i++ { // 10 entries >> batch of 3 (and 9 % 3 == 0 edge below)
		p := filepath.Join(root, fmt.Sprintf("f%02d", i))
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		want[p] = true
	}
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ { // exactly two full batches, then EOF
		p := filepath.Join(sub, fmt.Sprintf("g%d", i))
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		want[p] = true
	}
	var got []string
	err := walkDirBounded(root, func(p string, d stdfs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			got = append(got, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("visited %d files, want %d: %v", len(got), len(want), got)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected %s", p)
		}
		delete(want, p)
	}
	if len(want) != 0 {
		t.Errorf("missed %v", want)
	}
}

func TestWalkDirBounded_SmallDirectoryIsSortedAndMissingRootIsReported(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"c", "a", "b"} {
		if err := os.WriteFile(filepath.Join(root, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var names []string
	_ = walkDirBounded(root, func(p string, d stdfs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, d.Name())
		}
		return nil
	})
	if fmt.Sprint(names) != "[a b c]" {
		t.Errorf("order = %v", names)
	}
	var sawNotExist bool
	_ = walkDirBounded(filepath.Join(root, "nope"), func(p string, d stdfs.DirEntry, err error) error {
		sawNotExist = err != nil && os.IsNotExist(err)
		return nil
	})
	if !sawNotExist {
		t.Error("a missing root must be reported to the callback")
	}
}
