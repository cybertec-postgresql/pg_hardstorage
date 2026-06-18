package fs_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	stdio "io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

func openFS(t *testing.T) *fs.Plugin {
	t.Helper()
	root := t.TempDir()
	p := &fs.Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestOpen_RejectsHost(t *testing.T) {
	p := &fs.Plugin{}
	err := p.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Host: "remote.example.com", Path: "/tmp/x"}})
	if err == nil {
		t.Error("non-localhost host should be rejected")
	}
}

func TestOpen_AcceptsLocalhost(t *testing.T) {
	p := &fs.Plugin{}
	tmp := t.TempDir()
	err := p.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Host: "localhost", Path: tmp}})
	if err != nil {
		t.Errorf("localhost should be accepted: %v", err)
	}
}

func TestPutGet_RoundTrip(t *testing.T) {
	p := openFS(t)
	body := []byte("hello world")
	res, err := p.PutBytes(context.Background(), "foo/bar", body, storage.PutOptions{})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if res.Size != int64(len(body)) {
		t.Errorf("size = %d", res.Size)
	}
	want := sha256.Sum256(body)
	if res.ContentSHA256 != want {
		t.Errorf("hash mismatch")
	}

	rc, err := p.Get(context.Background(), "foo/bar")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer rc.Close()
	got, err := stdio.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("got %q want %q", got, body)
	}
}

func TestPut_IfNotExists(t *testing.T) {
	p := openFS(t)
	body := []byte("first")
	if _, err := p.PutBytes(context.Background(), "k", body, storage.PutOptions{IfNotExists: true}); err != nil {
		t.Fatalf("first put: %v", err)
	}
	_, err := p.PutBytes(context.Background(), "k", []byte("second"), storage.PutOptions{IfNotExists: true})
	if err != storage.ErrAlreadyExists {
		t.Errorf("expected ErrAlreadyExists; got %v", err)
	}
	// First object's content must still be intact.
	rc, _ := p.Get(context.Background(), "k")
	defer rc.Close()
	got, _ := stdio.ReadAll(rc)
	if string(got) != "first" {
		t.Errorf("second put should not have replaced; got %q", got)
	}
}

func TestPut_Overwrite_AtomicViaTmpRename(t *testing.T) {
	p := openFS(t)
	if _, err := p.PutBytes(context.Background(), "k", []byte("v1"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.PutBytes(context.Background(), "k", []byte("v2-much-longer"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	// No leftover .tmp file under root.
	tmpFound := false
	_ = filepath.WalkDir(p.Root(), func(path string, _ os.DirEntry, _ error) error {
		if filepath.Ext(path) == ".tmp" {
			tmpFound = true
		}
		return nil
	})
	if tmpFound {
		t.Error("a .tmp file leaked into the repository")
	}
	rc, _ := p.Get(context.Background(), "k")
	defer rc.Close()
	got, _ := stdio.ReadAll(rc)
	if string(got) != "v2-much-longer" {
		t.Errorf("got %q", got)
	}
}

func TestPut_ChecksumMismatch(t *testing.T) {
	p := openFS(t)
	wrong := [32]byte{1, 2, 3} // any non-zero, non-real hash
	_, err := p.PutBytes(context.Background(), "k", []byte("hello"), storage.PutOptions{ContentSHA256: wrong})
	if err != storage.ErrChecksumMismatch {
		t.Errorf("expected ErrChecksumMismatch; got %v", err)
	}
	// File must not exist after a checksum failure.
	if _, err := p.Stat(context.Background(), "k"); err != storage.ErrNotFound {
		t.Errorf("checksum-mismatched put should not leave a file; stat err=%v", err)
	}
}

func TestPut_ChecksumMatch(t *testing.T) {
	p := openFS(t)
	body := []byte("hello")
	hash := sha256.Sum256(body)
	res, err := p.PutBytes(context.Background(), "k", body, storage.PutOptions{ContentSHA256: hash})
	if err != nil {
		t.Fatal(err)
	}
	if res.ContentSHA256 != hash {
		t.Error("returned hash mismatch")
	}
}

func TestPut_ContentLengthMismatch(t *testing.T) {
	p := openFS(t)
	_, err := p.PutBytes(context.Background(), "k", []byte("hello"), storage.PutOptions{ContentLength: 999})
	if err == nil {
		t.Error("declared length 999 vs actual 5 should error")
	}
}

func TestStat_NotFound(t *testing.T) {
	p := openFS(t)
	if _, err := p.Stat(context.Background(), "missing"); err != storage.ErrNotFound {
		t.Errorf("expected ErrNotFound; got %v", err)
	}
}

func TestGet_NotFound(t *testing.T) {
	p := openFS(t)
	if _, err := p.Get(context.Background(), "missing"); err != storage.ErrNotFound {
		t.Errorf("expected ErrNotFound; got %v", err)
	}
}

func TestDelete_Idempotent(t *testing.T) {
	p := openFS(t)
	if _, err := p.PutBytes(context.Background(), "k", []byte("v"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(context.Background(), "k"); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(context.Background(), "k"); err != nil {
		t.Errorf("second delete should be a no-op; got %v", err)
	}
}

func TestRenameIfNotExists_Success(t *testing.T) {
	p := openFS(t)
	if _, err := p.PutBytes(context.Background(), "src", []byte("hi"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := p.RenameIfNotExists(context.Background(), "src", "dst"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Stat(context.Background(), "src"); err != storage.ErrNotFound {
		t.Errorf("src should be gone; got %v", err)
	}
	if _, err := p.Stat(context.Background(), "dst"); err != nil {
		t.Errorf("dst should exist: %v", err)
	}
}

func TestRenameIfNotExists_Conflict(t *testing.T) {
	p := openFS(t)
	if _, err := p.PutBytes(context.Background(), "src", []byte("a"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.PutBytes(context.Background(), "dst", []byte("b"), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	err := p.RenameIfNotExists(context.Background(), "src", "dst")
	if err != storage.ErrAlreadyExists {
		t.Errorf("expected ErrAlreadyExists; got %v", err)
	}
	// Both src and dst should be intact.
	if _, err := p.Stat(context.Background(), "src"); err != nil {
		t.Errorf("src should remain on conflict: %v", err)
	}
	if _, err := p.Stat(context.Background(), "dst"); err != nil {
		t.Errorf("dst should remain on conflict: %v", err)
	}
}

func TestList_RecursiveAndSorted(t *testing.T) {
	p := openFS(t)
	keys := []string{"a/1", "a/2", "b/1", "c/d/e/1"}
	for _, k := range keys {
		if _, err := p.PutBytes(context.Background(), k, []byte(k), storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for info, err := range p.List(context.Background(), "") {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, info.Key)
	}
	sort.Strings(got)
	want := []string{"a/1", "a/2", "b/1", "c/d/e/1"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q want %q", i, got[i], want[i])
		}
	}
}

func TestList_Prefix(t *testing.T) {
	p := openFS(t)
	for _, k := range []string{"a/1", "a/2", "b/1"} {
		if _, err := p.PutBytes(context.Background(), k, []byte("x"), storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for info, err := range p.List(context.Background(), "a") {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, info.Key)
	}
	if len(got) != 2 {
		t.Errorf("prefix=a should yield 2; got %v", got)
	}
}

func TestList_MissingPrefixIsEmpty(t *testing.T) {
	p := openFS(t)
	for info, err := range p.List(context.Background(), "no-such-dir") {
		t.Errorf("unexpected: %v %v", info, err)
	}
}

func TestKey_RejectsEscape(t *testing.T) {
	p := openFS(t)
	_, err := p.PutBytes(context.Background(), "../escape", []byte("x"), storage.PutOptions{})
	if err == nil {
		t.Error("key escaping the root should be rejected")
	}
}

func TestPut_Concurrent_IfNotExists_OnlyOneWins(t *testing.T) {
	p := openFS(t)
	const N = 16
	var wins atomic.Int32
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, err := p.PutBytes(context.Background(), "race", []byte("once"), storage.PutOptions{IfNotExists: true})
			if err == nil {
				wins.Add(1)
			} else if err != storage.ErrAlreadyExists {
				t.Errorf("unexpected err: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := wins.Load(); got != 1 {
		t.Errorf("expected exactly one winner; got %d", got)
	}
}

func TestSetRetention_Unsupported(t *testing.T) {
	p := openFS(t)
	if err := p.SetRetention(context.Background(), "k", time.Time{}, storage.WORMCompliance); err != storage.ErrUnsupported {
		t.Errorf("expected ErrUnsupported; got %v", err)
	}
}

// External review pass: fs plugin methods accepted ctx but never
// checked it. POSIX syscalls don't honour ctx natively — adding an
// early-bail check at each public entry-point is the operationally
// correct fix (in-flight syscalls remain uninterruptible, but
// already-cancelled callers get cancellation before we touch the
// kernel). Pin every entry point.
func TestFS_PreCancelledCtx_AllMethodsHonour(t *testing.T) {
	p := openFS(t)

	// Plant an object so Get / Stat / Delete / List / Rename have
	// something to act on.
	if _, err := p.Put(context.Background(), "k", bytes.NewReader([]byte("x")),
		storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("Put", func(t *testing.T) {
		_, err := p.Put(ctx, "kp", bytes.NewReader([]byte("x")), storage.PutOptions{})
		if err == nil {
			t.Error("Put should return ctx error on pre-cancelled ctx")
		}
	})
	t.Run("Get", func(t *testing.T) {
		_, err := p.Get(ctx, "k")
		if err == nil {
			t.Error("Get should return ctx error on pre-cancelled ctx")
		}
	})
	t.Run("Stat", func(t *testing.T) {
		_, err := p.Stat(ctx, "k")
		if err == nil {
			t.Error("Stat should return ctx error on pre-cancelled ctx")
		}
	})
	t.Run("Delete", func(t *testing.T) {
		err := p.Delete(ctx, "k")
		if err == nil {
			t.Error("Delete should return ctx error on pre-cancelled ctx")
		}
	})
	t.Run("RenameIfNotExists", func(t *testing.T) {
		err := p.RenameIfNotExists(ctx, "src", "dst")
		if err == nil {
			t.Error("RenameIfNotExists should return ctx error on pre-cancelled ctx")
		}
	})
	t.Run("List", func(t *testing.T) {
		var got error
		for _, err := range p.List(ctx, "") {
			if err != nil {
				got = err
				break
			}
		}
		if got == nil {
			t.Error("List should yield ctx error on pre-cancelled ctx")
		}
	})
}
