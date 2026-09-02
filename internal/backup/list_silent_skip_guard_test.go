package backup_test

// list_silent_skip_guard_test.go — a manifest that could not be read
// must not vanish without trace.
//
// ManifestStore.List yields (manifest, error) per entry, and every
// consumer decides what to do with a per-entry failure. Skipping is
// usually right: one corrupt manifest must not stop a fleet walk, a
// restore, or a menu. Skipping SILENTLY is not, and the same omission
// was written five separate times before anyone noticed:
//
//	restore.ResolveLatest          "latest" resolved to an older backup
//	restore.ResolveBackupForTime   PITR seeded from further back
//	timetravel.pickBackupForTarget seed chosen, then "no backup exists
//	                               that far back" reported
//	simple restore / verify flows  the menu quietly omitted backups
//	partial's latest resolvers     "no backups for deployment %q"
//
// Every one of them turned "I could not read this" into either a
// different answer or a claim that nothing was there. Five copies of one
// omission is a missing guard, so this is the guard.
//
// The rule is deliberately narrow. Reporting and aggregation walks
// (`cost`, `forecast`, `fleet search`, the compliance sections) may skip
// an unreadable manifest without ceremony — they summarise a population,
// and one missing row does not become a different answer. What must not
// skip silently is a function that CHOOSES a backup for the caller to
// act on: there the skipped manifest is not a missing row, it is
// possibly the answer.
//
// So the guard fires only on selector-shaped functions — names
// containing pick/resolve/latest/newest/best/choose — that range over a
// manifest iterator and whose error branch is nothing but a jump.
// Counting, printing, recording a finding, or returning all pass.
//
// Sibling tree-wide guards live in internal/fsutil
// (rename_dirsync_guard_test.go, write_close_guard_test.go).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// listSkipExempt maps a repo-relative path to why a bare skip is
// acceptable there. Keep it short and justified.
var listSkipExempt = map[string]string{}

func TestListConsumers_DoNotSkipUnreadableManifestsSilently(t *testing.T) {
	root := repoRootForGuard(t)
	fset := token.NewFileSet()

	type offender struct {
		file, fn string
		line     int
	}
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
		if _, ok := listSkipExempt[rel]; ok {
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
			if !isSelectorFunc(fn.Name.Name) {
				continue
			}
			for _, rng := range listRangeStmts(fn.Body) {
				if line, bad := bareSkipOnError(fset, rng); bad {
					offenders = append(offenders, offender{rel, fn.Name.Name, line})
				}
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
		b.WriteString("backup-selecting function discards a per-entry error with a bare jump:\n")
		for _, o := range offenders {
			b.WriteString("  - " + o.file + ":" + itoa(o.line) + " in " + o.fn + "()\n")
		}
		b.WriteString("\nThese functions CHOOSE a backup for the caller to act on, so a skipped " +
			"manifest is not a missing row — it is possibly the answer. One that fails " +
			"verification yields no timestamps and cannot be ranked, so dropping it silently " +
			"returns a different backup, or reports that none exists, with nothing to say " +
			"why. Count it, print it, record it as a finding, or return the error. If a bare " +
			"skip is genuinely correct here, add the file to listSkipExempt with a reason.")
		t.Fatal(b.String())
	}
}

// isSelectorFunc reports whether a function name reads as "choose one
// backup" rather than "summarise many".
func isSelectorFunc(name string) bool {
	l := strings.ToLower(name)
	for _, verb := range []string{"pick", "resolve", "latest", "newest", "best", "choose", "select"} {
		if strings.Contains(l, verb) {
			return true
		}
	}
	return false
}

// listRangeStmts returns range statements whose iterator is a
// List-style call yielding (value, error).
func listRangeStmts(body *ast.BlockStmt) []*ast.RangeStmt {
	var out []*ast.RangeStmt
	ast.Inspect(body, func(n ast.Node) bool {
		rng, ok := n.(*ast.RangeStmt)
		if !ok || rng.Key == nil || rng.Value == nil {
			return true
		}
		call, ok := rng.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		// ManifestStore.List / ListIncludingTombstoned /
		// ListAttestationless — the manifest iterators. Storage List is
		// deliberately out of scope: its error path is already required
		// to abort by the fail-closed convention, and its consumers are
		// a different population.
		switch sel.Sel.Name {
		case "List", "ListIncludingTombstoned", "ListAttestationless":
		default:
			return true
		}
		// The value variable must be an error-ish name, which is what
		// distinguishes a (manifest, error) iterator from a plain slice.
		id, ok := rng.Value.(*ast.Ident)
		if !ok || !strings.Contains(strings.ToLower(id.Name), "err") {
			return true
		}
		out = append(out, rng)
		return true
	})
	return out
}

// bareSkipOnError reports whether the loop's `if <err> != nil` branch
// consists of nothing but a continue/break.
func bareSkipOnError(fset *token.FileSet, rng *ast.RangeStmt) (int, bool) {
	errName := ""
	if id, ok := rng.Value.(*ast.Ident); ok {
		errName = id.Name
	}
	found := 0
	bad := false
	for _, stmt := range rng.Body.List {
		ifs, ok := stmt.(*ast.IfStmt)
		if !ok {
			continue
		}
		bin, ok := ifs.Cond.(*ast.BinaryExpr)
		if !ok || bin.Op != token.NEQ {
			continue
		}
		// Match `<err> != nil` and `<err> != nil || <x> == nil`-style
		// conditions by looking for the error identifier anywhere in
		// the condition.
		if !exprMentions(ifs.Cond, errName) {
			continue
		}
		if len(ifs.Body.List) != 1 {
			continue
		}
		if br, ok := ifs.Body.List[0].(*ast.BranchStmt); ok &&
			(br.Tok == token.CONTINUE || br.Tok == token.BREAK) {
			found = fset.Position(ifs.Pos()).Line
			bad = true
		}
	}
	return found, bad
}

func exprMentions(e ast.Expr, name string) bool {
	if name == "" {
		return false
	}
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return true
	})
	return found
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func repoRootForGuard(t *testing.T) string {
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

// The guard is only worth keeping if it fires on the shape the six
// fixed bugs had and stays quiet on the shapes around them. Both
// directions matter: a guard that flags every aggregation walk in the
// tree produces an exemption list longer than its findings and gets
// deleted.
func TestListSkipGuard_FiresOnTheShapeItWasWrittenFor(t *testing.T) {
	cases := map[string]struct {
		src  string
		want bool
	}{
		"selector with a bare continue (all six bugs)": {`package p
func resolveLatest(store *S, dep string) (string, error) {
	var best string
	for m, err := range store.List(ctx, dep, v) {
		if err != nil {
			continue
		}
		best = m.BackupID
	}
	return best, nil
}`, true},
		"selector that counts the skip": {`package p
func resolveLatest(store *S, dep string) (string, error) {
	skipped := 0
	for m, err := range store.List(ctx, dep, v) {
		if err != nil {
			skipped++
			continue
		}
		_ = m
	}
	return "", nil
}`, false},
		"selector that returns the error": {`package p
func pickLatest(store *S, dep string) (string, error) {
	for m, err := range store.List(ctx, dep, v) {
		if err != nil {
			return "", err
		}
		_ = m
	}
	return "", nil
}`, false},
		"aggregation walk may skip freely": {`package p
func Compute(store *S, dep string) int {
	n := 0
	for m, err := range store.List(ctx, dep, v) {
		if err != nil {
			continue
		}
		n += len(m.Files)
	}
	return n
}`, false},
		"selector over a plain slice is not a manifest iterator": {`package p
func pickLatest(items []*M) string {
	for i, err := range items {
		if err != nil {
			continue
		}
		_ = i
	}
	return ""
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
				if !ok || fn.Body == nil || !isSelectorFunc(fn.Name.Name) {
					continue
				}
				for _, rng := range listRangeStmts(fn.Body) {
					if _, bad := bareSkipOnError(fset, rng); bad {
						got = true
					}
				}
			}
			if got != tc.want {
				t.Errorf("flagged=%v, want %v", got, tc.want)
			}
		})
	}
}
