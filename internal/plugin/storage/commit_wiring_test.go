package storage_test

// commit_wiring_test.go — every exclusive publish must go through
// CommitExclusive.
//
// Converting the known writers is not the durable fix. Issue #45's
// reporter concluded "only the manifest commit deletes", and that was
// wrong by four subsystems — integrity, dsa and threshold (twice) all
// staged through `<key>.tmp.<rand>` + RenameIfNotExists too, each
// costing a DELETE per commit on S3. The next writer will be added the
// same way unless something objects.
//
// So this asserts the invariant rather than the instances: production
// code outside the storage package does not call RenameIfNotExists.
// That is the primitive with the delete in it; CommitExclusive is the
// one place allowed to reach for it, and only where the backend cannot
// commit conditionally.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// callsRenameIfNotExists reports the functions in a file that call it.
func callsRenameIfNotExists(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil // not parseable as Go; nothing to assert
	}
	var hits []string
	var fn string
	ast.Inspect(f, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.FuncDecl:
			fn = d.Name.Name
		case *ast.CallExpr:
			sel, ok := d.Fun.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "RenameIfNotExists" {
				hits = append(hits, fn)
			}
		}
		return true
	})
	return hits
}

// TestOnlyCommitExclusiveRenames is the invariant.
func TestOnlyCommitExclusiveRenames(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", ".."))

	// The storage package defines and implements the primitive; the
	// plugins implement it; the contract suite and fault middleware
	// exercise it. Everything else must go through CommitExclusive.
	allowedDirs := []string{
		filepath.Join("internal", "plugin", "storage"),
	}

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if info != nil && info.IsDir() {
				switch info.Name() {
				case "vendor", ".git", "test-runs", "node_modules":
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		for _, d := range allowedDirs {
			if strings.HasPrefix(rel, d) {
				return nil
			}
		}
		for _, fn := range callsRenameIfNotExists(t, path) {
			offenders = append(offenders, rel+": "+fn)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d production call site(s) use RenameIfNotExists directly:\n  %s\n\n"+
			"That primitive stages through a temporary and DELETES it — on S3 it is "+
			"HeadObject + CopyObject + DeleteObject — so every commit emits a delete "+
			"marker and the whole path additionally requires a conditional COPY that many "+
			"S3-compatible stores do not implement (issue #45).\n\n"+
			"Publish through storage.CommitExclusive instead: one conditional PUT where "+
			"the backend supports it, staging only where it does not.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
