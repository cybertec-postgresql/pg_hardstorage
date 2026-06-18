package fsutil

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileSync_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	want := []byte("hello")
	if err := WriteFileSync(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("content = %q, want %q", got, want)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", st.Mode().Perm())
	}
}

func TestWriteFileSync_OverwritesExistingInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	if err := WriteFileSync(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileSync(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Fatalf("after overwrite: %q, want second", got)
	}
}

func TestWriteFileSync_NonExistentParentRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nope", "x.txt")
	err := WriteFileSync(path, []byte("x"), 0o600)
	if err == nil {
		t.Fatal("expected error for missing parent")
	}
}

func TestWriteFileAtomic_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := WriteFileAtomic(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ok" {
		t.Fatalf("content = %q, want ok", got)
	}
	// No leftover tmp.
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("tmp should be gone; stat err = %v", err)
	}
}

func TestWriteFileAtomic_RewritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := WriteFileAtomic(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "v2" {
		t.Fatalf("after rewrite: %q, want v2", got)
	}
}

func TestWriteFileAtomic_StaleTmpRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	tmp := path + ".tmp"
	// Plant a stale tmp from a prior crash.
	if err := os.WriteFile(tmp, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := WriteFileAtomic(path, []byte("ok"), 0o600)
	if err == nil {
		t.Fatal("expected error when stale tmp blocks O_EXCL open")
	}
	// Stale tmp must remain — caller has to acknowledge it.
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("stale tmp should still exist: %v", err)
	}
}

func TestSyncDir_HappyPath(t *testing.T) {
	dir := t.TempDir()
	if err := SyncDir(dir); err != nil {
		t.Fatalf("syncdir: %v", err)
	}
}

func TestSyncDir_MissingIsNoOp(t *testing.T) {
	if err := SyncDir(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("missing dir should be silent: %v", err)
	}
}

func TestSyncFile_Nil(t *testing.T) {
	if err := SyncFile(nil); err == nil {
		t.Fatal("nil file should error")
	}
}

func TestSyncFile_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := SyncFile(f); err != nil {
		t.Fatalf("syncfile: %v", err)
	}
}
