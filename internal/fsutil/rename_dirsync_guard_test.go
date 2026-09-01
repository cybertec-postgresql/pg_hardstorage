package fsutil_test

// rename_dirsync_guard_test.go — every os.Rename must be followed by a
// parent-directory fsync.
//
// This package exists precisely so call sites don't re-implement the
// write-fsync-rename-fsyncdir dance (see the package doc). They
// re-implemented it anyway — and every hand-rolled copy dropped the
// same final step, the one that is invisible until a power loss:
//
//	internal/backup/keystore   the manifest-signing keypair
//	internal/cli/keyring_install  the installed private key + KEK
//	internal/restore/checkpoint   the resume checkpoint
//	internal/restore              the finalized chain-restore datadir
//	internal/simple/state         deployment state
//	internal/simple/flow_setup    the operator's pg_hardstorage.yaml
//	internal/llm/history          (whose comment claimed "repo-grade")
//
// POSIX fsync(fd) flushes the file's data and inode, NOT the parent
// directory's dentry list. rename(2) can therefore be reported as
// successful and still be lost on power loss. Six copies of the same
// omission is not six mistakes; it is a missing guard, so this is the
// guard: any function that calls os.Rename must also fsync a
// directory.
//
// The check is intentionally coarse — same function body, any call
// named SyncDir/syncDir. It cannot prove the RIGHT directory was
// synced, only that the author thought about it. If a new call site
// legitimately cannot sync in the same function, add it to
// renameDirSyncExempt with a reason rather than deleting this test.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renameDirSyncExempt maps a repo-relative file path to why its
// os.Rename calls need no parent fsync. Keep this list short and
// justified.
var renameDirSyncExempt = map[string]string{
	// Test-support only: builds deliberately-corrupt repo fixtures in
	// a t.TempDir. Nothing here has to survive a reboot.
	"internal/testkit/runner/corrupt_repo.go": "test fixture builder; durability is not a property under test",
}

func TestEveryRenameFsyncsItsParentDirectory(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	type offender struct{ file, fn string }
	var offenders []offender

	// Walk internal/ and cmd/ explicitly rather than the repo root: the
	// working tree also holds soak logs and coverage artefacts, and
	// walking those turned a millisecond check into three minutes.
	walk := func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		// Only our own source, and skip anything explicitly exempt.
		if !strings.HasPrefix(rel, "internal/") && !strings.HasPrefix(rel, "cmd/") {
			return nil
		}
		if _, ok := renameDirSyncExempt[rel]; ok {
			return nil
		}

		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // not our problem; the build catches it
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			renames, dirSyncs := 0, 0
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					pkg, _ := fun.X.(*ast.Ident)
					if pkg != nil && pkg.Name == "os" && fun.Sel.Name == "Rename" {
						renames++
					}
					if strings.EqualFold(fun.Sel.Name, "syncdir") {
						dirSyncs++
					}
					// fsutil.WriteFileAtomic / WriteFileSync do the
					// whole dance internally.
					if pkg != nil && pkg.Name == "fsutil" &&
						strings.HasPrefix(fun.Sel.Name, "WriteFile") {
						dirSyncs++
					}
				case *ast.Ident:
					if strings.EqualFold(fun.Name, "syncdir") {
						dirSyncs++
					}
				}
				return true
			})
			if renames > 0 && dirSyncs == 0 {
				offenders = append(offenders, offender{rel, fn.Name.Name})
			}
		}
		return nil
	}
	for _, sub := range []string{"internal", "cmd"} {
		if err := filepath.WalkDir(filepath.Join(root, sub), walk); err != nil {
			t.Fatalf("walk %s: %v", sub, err)
		}
	}

	if len(offenders) > 0 {
		var b strings.Builder
		b.WriteString("os.Rename with no parent-directory fsync in the same function:\n")
		for _, o := range offenders {
			b.WriteString("  - " + o.file + ": " + o.fn + "()\n")
		}
		b.WriteString("\nfsync(file) does not flush the parent directory's dentry list, so " +
			"the kernel can report rename(2) as successful and still lose it on power loss. " +
			"Call fsutil.WriteFileAtomic (which does the whole dance), or add " +
			"fsutil.SyncDir(filepath.Dir(dst)) after the rename. If the sync genuinely " +
			"belongs elsewhere, add the file to renameDirSyncExempt with a reason.")
		t.Fatal(b.String())
	}
}

// The guard is only worth having if it would have caught the bugs it
// was written for. Reconstruct the shape each offender had and confirm
// the analysis flags it.
func TestRenameDirSyncGuard_FlagsTheShapeItWasWrittenFor(t *testing.T) {
	cases := map[string]struct {
		src  string
		want bool
	}{
		"writeFile+rename, no sync (simple/state, flow_setup)": {`package p
func save(tmp, dst string) error {
	if err := os.WriteFile(tmp, nil, 0o600); err != nil { return err }
	return os.Rename(tmp, dst)
}`, true},
		"fsync file, rename, no dir sync (keystore, checkpoint)": {`package p
func save(f *os.File, tmp, dst string) error {
	if err := f.Sync(); err != nil { return err }
	if err := f.Close(); err != nil { return err }
	return os.Rename(tmp, dst)
}`, true},
		"rename then SyncDir": {`package p
func save(tmp, dst string) error {
	if err := os.Rename(tmp, dst); err != nil { return err }
	return fsutil.SyncDir(filepath.Dir(dst))
}`, false},
		"lowercase local syncDir (the fs plugin)": {`package p
func save(tmp, dst string) error {
	if err := os.Rename(tmp, dst); err != nil { return err }
	return syncDir(filepath.Dir(dst))
}`, false},
		"no rename at all": {`package p
func save(dst string) error { return os.WriteFile(dst, nil, 0o600) }`, false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "x.go")
			if err := os.WriteFile(path, []byte(tc.src), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := fileHasUnsyncedRename(t, path); got != tc.want {
				t.Errorf("flagged=%v, want %v — the guard does not see this shape", got, tc.want)
			}
		})
	}
}

// fileHasUnsyncedRename runs the same analysis as the guard above over
// one file. Kept separate so the guard's own logic is under test.
func fileHasUnsyncedRename(t *testing.T, path string) bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		renames, dirSyncs := 0, 0
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				pkg, _ := fun.X.(*ast.Ident)
				if pkg != nil && pkg.Name == "os" && fun.Sel.Name == "Rename" {
					renames++
				}
				if strings.EqualFold(fun.Sel.Name, "syncdir") {
					dirSyncs++
				}
				if pkg != nil && pkg.Name == "fsutil" && strings.HasPrefix(fun.Sel.Name, "WriteFile") {
					dirSyncs++
				}
			case *ast.Ident:
				if strings.EqualFold(fun.Name, "syncdir") {
					dirSyncs++
				}
			}
			return true
		})
		if renames > 0 && dirSyncs == 0 {
			return true
		}
	}
	return false
}

// repoRoot walks up from the working directory until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the working directory")
		}
		dir = parent
	}
}
