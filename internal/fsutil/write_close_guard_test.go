package fsutil_test

// write_close_guard_test.go — a file opened for WRITING must have its
// Close checked, not merely deferred.
//
// `defer f.Close()` is right for a reader and wrong for a writer.
// Close(2) is where a delayed-allocation, quota-bound or network
// filesystem finally reports ENOSPC / EDQUOT / EIO for writes the
// kernel accepted earlier. A deferred Close discards that error, so the
// function returns success over a file that is short or empty.
//
// The site that motivated this: keystore.LoadOrGenerateKEK generated the
// repository's KEK, wrote it, fsynced it — and closed it with a bare
// `defer`. It then returned the key IN MEMORY, which the caller
// immediately used to wrap a DEK and encrypt a backup. Had the Close
// failed, that backup would have been sealed under a key whose only
// on-disk copy never landed: unrecoverable, with nothing to notice at
// the time.
//
// What counts as CHECKED is the result being consumed by something
// other than the blank identifier: `return f.Close()`,
// `if err := f.Close(); err != nil`, or the deferred-capture idiom the
// restore paths use —
//
//	defer func() {
//		if cerr := dst.Close(); cerr != nil && err == nil {
//			err = fmt.Errorf("close destination: %w", cerr)
//		}
//	}()
//
// — which is deferred but emphatically not discarded. A bare
// `defer f.Close()` or `_ = f.Close()` is not.
//
// The guard is scoped tightly to the shape the bug had: the file's
// only Close is a DEFERRED one whose result is thrown away. That keeps
// it free of the legitimate patterns around it —
//
//   - a function that opens a writer and hands it to a caller
//     (checkpoint.NewWriter, cef.Sink) has transferred ownership; the
//     eventual Close is the owner's problem;
//   - a Close on a failure path, right before returning the error that
//     already describes the failure (profile.startProfiling), is not
//     hiding anything.
//
// A guard that fires on those becomes noise, and a noisy guard gets
// deleted.
//
// Read-only opens are not considered: os.Open, os.ReadFile and
// O_RDONLY-only OpenFile calls never reach this analysis.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCloseExempt maps a repo-relative path to why a deferred-only
// Close is acceptable there.
var writeCloseExempt = map[string]string{}

func TestWritableFilesHaveTheirCloseChecked(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	type offender struct{ file, fn string }
	var offenders []offender

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
		if _, ok := writeCloseExempt[rel]; ok {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if opensForWriting(fn.Body) && deferredDiscardedCloseOnly(fn.Body) {
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
		b.WriteString("file opened for writing whose only Close is deferred and discarded:\n")
		for _, o := range offenders {
			b.WriteString("  - " + o.file + ": " + o.fn + "()\n")
		}
		b.WriteString("\nClose(2) is where a write that the kernel accepted earlier " +
			"finally reports ENOSPC/EDQUOT/EIO. Discarding it lets the function return " +
			"success over a short or empty file. Either call Close explicitly and check " +
			"it, or capture it through the named error return in the deferred close (see " +
			"restore.materializeFile), or use fsutil.WriteFileSync / WriteFileAtomic. If " +
			"a discarded Close is genuinely right here, add the file to writeCloseExempt " +
			"with a reason.")
		t.Fatal(b.String())
	}
}

// opensForWriting reports whether body contains an os.Create, or an
// os.OpenFile whose flag expression mentions a write flag.
func opensForWriting(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, _ := sel.X.(*ast.Ident)
		if pkg == nil || pkg.Name != "os" {
			return true
		}
		switch sel.Sel.Name {
		case "Create":
			found = true
		case "OpenFile":
			if len(call.Args) >= 2 && mentionsWriteFlag(call.Args[1]) {
				found = true
			}
		}
		return true
	})
	return found
}

// mentionsWriteFlag reports whether a flags expression references any
// os.O_* constant that implies writing.
func mentionsWriteFlag(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, _ := sel.X.(*ast.Ident)
		if pkg == nil || pkg.Name != "os" {
			return true
		}
		switch sel.Sel.Name {
		case "O_WRONLY", "O_RDWR", "O_APPEND", "O_CREATE", "O_TRUNC":
			found = true
		}
		return true
	})
	return found
}

// deferredDiscardedCloseOnly reports whether body's only handling of
// Close is a deferred call whose result is thrown away.
//
// True means: at least one Close sits inside a defer with its error
// discarded, and no Close anywhere in the function has its result
// consumed. Both halves matter — the first is the bug's shape, the
// second lets the deferred-capture idiom and any explicit checked Close
// clear the function.
func deferredDiscardedCloseOnly(body *ast.BlockStmt) bool {
	consumed := consumedExprs(body)
	inDefer := map[ast.Node]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		ast.Inspect(d, func(m ast.Node) bool { inDefer[m] = true; return true })
		return true
	})

	deferredDiscarded, anyConsumed := false, false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Close" {
			return true
		}
		switch {
		case consumed[ast.Expr(call)]:
			anyConsumed = true
		case inDefer[n]:
			deferredDiscarded = true
		}
		return true
	})
	return deferredDiscarded && !anyConsumed
}

// consumedExprs collects expressions whose value is used: assigned to a
// non-blank target, returned, tested in an if, passed as an argument,
// or compared.
func consumedExprs(body *ast.BlockStmt) map[ast.Node]bool {
	consumed := map[ast.Node]bool{}
	mark := func(e ast.Expr) {
		if e != nil {
			consumed[e] = true
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch st := n.(type) {
		case *ast.AssignStmt:
			nonBlank := false
			for _, lhs := range st.Lhs {
				if id, ok := lhs.(*ast.Ident); !ok || id.Name != "_" {
					nonBlank = true
				}
			}
			if nonBlank {
				for _, rhs := range st.Rhs {
					mark(rhs)
				}
			}
		case *ast.ReturnStmt:
			for _, r := range st.Results {
				mark(r)
			}
		case *ast.IfStmt:
			mark(st.Cond)
		case *ast.CallExpr:
			for _, a := range st.Args {
				mark(a)
			}
		case *ast.BinaryExpr:
			mark(st.X)
			mark(st.Y)
		}
		return true
	})
	return consumed
}

func TestWriteCloseGuard_FlagsTheShapeItWasWrittenFor(t *testing.T) {
	cases := map[string]struct {
		src  string
		want bool
	}{
		"O_CREATE|O_WRONLY with deferred-only Close (the KEK bug)": {`package p
func gen(path string, k []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil { return err }
	defer f.Close()
	if _, err := f.Write(k); err != nil { return err }
	return f.Sync()
}`, true},
		"os.Create with deferred-only Close": {`package p
func w(path string) error {
	f, err := os.Create(path)
	if err != nil { return err }
	defer f.Close()
	_, err = f.Write(nil)
	return err
}`, true},
		"Close discarded to the blank identifier": {`package p
func w(path string) error {
	f, err := os.Create(path)
	if err != nil { return err }
	defer func() { _ = f.Close() }()
	return nil
}`, true},
		"checked Close on the success path": {`package p
func w(path string) error {
	f, err := os.Create(path)
	if err != nil { return err }
	if _, err := f.Write(nil); err != nil { _ = f.Close(); return err }
	return f.Close()
}`, false},
		"deferred capture through the named error return (restore.materializeFile)": {`package p
func w(path string) (err error) {
	f, oerr := os.Create(path)
	if oerr != nil { return oerr }
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil { err = cerr }
	}()
	return nil
}`, false},
		"ownership handed to the caller — no Close here at all": {`package p
func NewWriter(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
}`, false},
		"Close only on a failure path, before returning the error (profile.startProfiling)": {`package p
func start(path string) (*os.File, error) {
	f, err := os.Create(path)
	if err != nil { return nil, err }
	if err := begin(f); err != nil { _ = f.Close(); return nil, err }
	return f, nil
}`, false},
		"long-lived writer closed by its owner (cef.Sink)": {`package p
func (s *Sink) Emit(line string) error {
	if s.w == nil {
		f, err := os.OpenFile(s.dest, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil { return err }
		s.w = f
	}
	if _, err := io.WriteString(s.w, line); err != nil {
		s.w.Close()
		s.w = nil
		return err
	}
	return nil
}`, false},
		"read-only open is not our business": {`package p
func r(path string) error {
	f, err := os.Open(path)
	if err != nil { return err }
	defer f.Close()
	return nil
}`, false},
		"O_RDONLY OpenFile is not flagged": {`package p
func r(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil { return err }
	defer f.Close()
	return nil
}`, false},
	}

	fset := token.NewFileSet()
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, err := parser.ParseFile(fset, "x.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got := false
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				if opensForWriting(fn.Body) && deferredDiscardedCloseOnly(fn.Body) {
					got = true
				}
			}
			if got != tc.want {
				t.Errorf("flagged=%v, want %v — the guard does not see this shape", got, tc.want)
			}
		})
	}
}
