package contract

// prune.go — the storage.EmptyDirPruner clauses, shared by every
// hierarchical backend (fs is the reference; sftp and scp are held to
// the same behaviour).
//
// Unlike Run these take an opened plugin rather than an opener: the
// sftp/scp bindings pay for a container per plugin, and the cases keep
// apart by working under their own top-level prefix.
//
// The cases look at directories through Stat. A hierarchical backend
// answers Stat for a directory the way it does for an object, which is
// the only window onto directories the StoragePlugin interface offers —
// List yields objects only.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

func emptyDirPruner(t *testing.T, sp storage.StoragePlugin) storage.EmptyDirPruner {
	t.Helper()
	pr, ok := sp.(storage.EmptyDirPruner)
	if !ok {
		t.Fatalf("%s does not implement storage.EmptyDirPruner", sp.Name())
	}
	return pr
}

func dirExists(t *testing.T, sp storage.StoragePlugin, dir string) bool {
	t.Helper()
	_, err := sp.Stat(context.Background(), dir)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("stat %q: %v", dir, err)
	}
	return err == nil
}

// emptyDir leaves dir behind as an empty directory, the way gc does: an
// object is written into it and deleted again.
func emptyDir(t *testing.T, sp storage.StoragePlugin, dir string) {
	t.Helper()
	putString(t, sp, dir+"/obj", "x")
	if err := sp.Delete(context.Background(), dir+"/obj"); err != nil {
		t.Fatalf("delete %q: %v", dir+"/obj", err)
	}
	if !dirExists(t, sp, dir) {
		t.Fatalf("setup: %q is not there after its only object was deleted", dir)
	}
}

func prune(t *testing.T, sp storage.StoragePlugin, dir string, includeSelf bool, want int) {
	t.Helper()
	n, err := emptyDirPruner(t, sp).PruneEmptyDirs(context.Background(), dir, includeSelf)
	if err != nil {
		t.Fatalf("PruneEmptyDirs(%q, %v): %v", dir, includeSelf, err)
	}
	if n != want {
		t.Fatalf("PruneEmptyDirs(%q, %v) removed %d directories, want %d", dir, includeSelf, n, want)
	}
}

// PruneEmptyDirs drives the EmptyDirPruner clauses against sp: only
// empty directories go, deepest first and in one pass; includeSelf
// decides about dir itself; the repo root always stays; a missing dir is
// not an error; names are data, whatever characters they hold.
func PruneEmptyDirs(t *testing.T, sp storage.StoragePlugin) {
	pr := emptyDirPruner(t, sp)
	ctx := context.Background()

	// First, while the repository holds nothing else: with every
	// directory in it empty, the root is empty too once they are gone —
	// and must still be standing.
	t.Run("RootIsNeverRemoved", func(t *testing.T) {
		// Whatever an earlier test left behind; also: pruning a root
		// that may not exist yet is not an error.
		if _, err := pr.PruneEmptyDirs(ctx, "", true); err != nil {
			t.Fatalf("PruneEmptyDirs on the root: %v", err)
		}
		emptyDir(t, sp, "prune-root/a/b")
		// b, a, prune-root. One more would be the root itself.
		prune(t, sp, "", true, 3)
		if dirExists(t, sp, "prune-root") {
			t.Fatal("empty directory under the root survived")
		}
		putString(t, sp, "prune-root/after", "still usable")
		if got := getString(t, sp, "prune-root/after"); got != "still usable" {
			t.Fatalf("repository unusable after pruning the root: got %q", got)
		}
		if err := sp.Delete(ctx, "prune-root/after"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("OnlyEmptyDirsBottomUp", func(t *testing.T) {
		const p = "prune-basic/chunks"
		putString(t, sp, p+"/sha256/aa/bb/keep", "kept")
		emptyDir(t, sp, p+"/sha256/cc/dd")
		prune(t, sp, p, false, 2) // dd, then the cc it leaves empty
		if dirExists(t, sp, p+"/sha256/cc") {
			t.Fatal("empty directory survived")
		}
		if got := getString(t, sp, p+"/sha256/aa/bb/keep"); got != "kept" {
			t.Fatalf("object in a non-empty directory damaged: got %q", got)
		}
		if !dirExists(t, sp, p) {
			t.Fatal("prefix itself must stay")
		}
		prune(t, sp, p, false, 0) // nothing left to do
	})

	t.Run("IncludeSelf", func(t *testing.T) {
		const p = "prune-self"
		emptyDir(t, sp, p+"/x/y/z")
		prune(t, sp, p+"/x", false, 2) // z, y — not x
		if !dirExists(t, sp, p+"/x") {
			t.Fatal("includeSelf=false removed the directory itself")
		}
		prune(t, sp, p+"/x", true, 1)
		if dirExists(t, sp, p+"/x") {
			t.Fatal("includeSelf=true left the empty directory itself")
		}
		if !dirExists(t, sp, p) {
			t.Fatal("pruning a directory removed its parent")
		}
		// Gone already — as after losing a race against another prune.
		prune(t, sp, p+"/x", true, 0)
		prune(t, sp, "/"+p+"/", true, 1) // slashes around dir are tolerated
	})

	t.Run("MissingDir", func(t *testing.T) {
		prune(t, sp, "prune-missing/never/created", true, 0)
		prune(t, sp, "prune-missing/never/created", false, 0)
	})

	t.Run("NonEmptyIsKept", func(t *testing.T) {
		const p = "prune-kept"
		putString(t, sp, p+"/a/b/obj", "kept")
		// Not an object the repository knows, but not ours to judge: any
		// file pins its directory.
		putString(t, sp, p+"/a/c/.hidden", "kept too")
		emptyDir(t, sp, p+"/a/d")
		prune(t, sp, p, true, 1) // d only
		for _, key := range []string{p + "/a/b/obj", p + "/a/c/.hidden"} {
			if _, err := sp.Stat(ctx, key); err != nil {
				t.Fatalf("%q after prune: %v", key, err)
			}
		}
	})

	// The writers' retry is for a directory that vanished. A write that
	// fails for another reason must still fail — promptly, and without
	// disturbing what is in the way.
	t.Run("RetryDoesNotMaskRealFailure", func(t *testing.T) {
		const p = "prune-blocked"
		putString(t, sp, p+"/file", "an object, not a directory")
		putString(t, sp, p+"/src", "src")
		if _, err := sp.Put(ctx, p+"/file/sub/obj", strings.NewReader("x"), storage.PutOptions{}); err == nil {
			t.Fatal("Put below an object succeeded")
		}
		if err := sp.RenameIfNotExists(ctx, p+"/src", p+"/file/sub/dst"); err == nil {
			t.Fatal("RenameIfNotExists to below an object succeeded")
		}
		if got := getString(t, sp, p+"/file"); got != "an object, not a directory" {
			t.Fatalf("object in the way was damaged: got %q", got)
		}
		if got := getString(t, sp, p+"/src"); got != "src" {
			t.Fatalf("source of the failed rename was damaged: got %q", got)
		}
	})

	t.Run("NamesAreData", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("these names are not valid on Windows filesystems")
		}
		const p = "prune-names"
		names := []string{
			"sp ace", "it's", `dq"dq`, "$(touch pwned)", "`touch pwned`",
			";x&y|z", "-rf", "*", "{}", "a\nb", "tab\there", `back\slash`, "ünï",
		}
		for _, n := range names {
			emptyDir(t, sp, p+"/"+n+"/sub dir")
		}
		const keep = p + "/keep $(x) 'q' *"
		putString(t, sp, keep+"/-obj", "kept")
		prune(t, sp, p, false, 2*len(names))
		for _, n := range names {
			if dirExists(t, sp, p+"/"+n) {
				t.Errorf("empty directory %q survived", n)
			}
		}
		if got := getString(t, sp, keep+"/-obj"); got != "kept" {
			t.Fatalf("object beside the pruned directories damaged: got %q", got)
		}
	})
}

// PruneRacesWriters runs writers against pruners: a Put or
// RenameIfNotExists whose directory is pruned under it must neither
// fail nor lose its object, and a prune that finds a directory filled
// or already gone must not report an error.
//
// Each writer works through a handful of bucket directories and deletes
// what it wrote straight away, so the pruners keep finding the very
// directories the writers are about to use empty.
func PruneRacesWriters(t *testing.T, sp storage.StoragePlugin, writers, iters int) {
	pr := emptyDirPruner(t, sp)
	ctx := context.Background()
	const prefix = "prune-race"

	read := func(key string) (string, error) {
		rc, err := sp.Get(ctx, key)
		if err != nil {
			return "", err
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		return string(b), err
	}

	stop := make(chan struct{})
	var pruners sync.WaitGroup
	// Two, so a prune also meets directories the other one just removed.
	for i := 0; i < 2; i++ {
		pruners.Add(1)
		go func() {
			defer pruners.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := pr.PruneEmptyDirs(ctx, prefix, false); err != nil {
					t.Errorf("prune racing writers: %v", err)
					return
				}
			}
		}()
	}

	var wg sync.WaitGroup
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				key := fmt.Sprintf("%s/sha256/%02x/%02x/g%d-%d", prefix, i%7, i%3, g, i)
				body := "body of " + key
				if _, err := sp.Put(ctx, key, strings.NewReader(body),
					storage.PutOptions{IfNotExists: i%2 == 0}); err != nil {
					t.Errorf("put %s: %v", key, err)
					return
				}
				if i%5 == 0 {
					// Into another bucket, whose directory the rename
					// has to create — and may see pruned again.
					dst := fmt.Sprintf("%s/sha256/%02x/%02x/g%d-%d-moved", prefix, (i+3)%7, (i+1)%3, g, i)
					if err := sp.RenameIfNotExists(ctx, key, dst); err != nil {
						t.Errorf("rename %s -> %s: %v", key, dst, err)
						return
					}
					key = dst
				}
				if got, err := read(key); err != nil || got != body {
					t.Errorf("object %s lost after a successful write: %q, %v", key, got, err)
					return
				}
				if err := sp.Delete(ctx, key); err != nil {
					t.Errorf("delete %s: %v", key, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	pruners.Wait()
	if t.Failed() {
		return
	}

	// Every object was deleted again, so the whole tree must fold up. A
	// directory that stays is pinned by something a writer left behind
	// (a staging file its retry abandoned).
	if _, err := pr.PruneEmptyDirs(ctx, prefix, true); err != nil {
		t.Fatalf("final prune: %v", err)
	}
	if dirExists(t, sp, prefix) {
		t.Fatalf("%q did not prune away after the race: a writer left something behind", prefix)
	}
}
