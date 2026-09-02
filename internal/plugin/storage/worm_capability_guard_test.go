package storage_test

// worm_capability_guard_test.go — a backend that implements WORM must
// declare it.
//
// The Capabilities().WORM bit is not documentation. Two safety gates
// read it and REFUSE when it is false:
//
//	repo/cas.go       retention configured + !Capabilities().WORM
//	                  => retentionUnenforceable, PutChunk refuses
//	repo/replicate.go same, for the destination repo
//
// Both exist to stop an operator believing a backup is immutable when
// it is not. So a backend that DOES enforce WORM but reports false
// inverts them: the compliance operator cannot take a WORM backup at
// all, is told their backend cannot enforce WORM — which is false — and
// the documented remedy is the flag that disables the guard.
//
// That is what azblob and gcs did. Both implement SetRetention against
// real object-level immutability (Azure SetImmutabilityPolicy, GCS
// ObjectRetention), and both Put implementations apply the deadline
// inline — azblob's carries a comment explaining that the CAS "relies
// on Put to enforce it" — while Capabilities() omitted the bit. The
// implementation was complete and deliberate; only the declaration
// was missing.
//
// This guard reads the source rather than a live backend: a
// SetRetention that is anything other than an unconditional
// "return storage.ErrUnsupported" is an implementation, and its
// Capabilities must say WORM: true.
//
// Middlewares (faultinject, throttle) are skipped, and by a property
// rather than a name list: their Capabilities DELEGATES to the wrapped
// plugin instead of returning a literal, so whatever the real backend
// declares is what they report. Only a package that states its own
// capabilities can misstate them.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackends_ImplementingRetentionDeclareWORM(t *testing.T) {
	root := storageRootForGuard(t)
	fset := token.NewFileSet()

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		files, ferr := filepath.Glob(filepath.Join(dir, "*.go"))
		if ferr != nil {
			t.Fatal(ferr)
		}
		var implementsRetention, declaresWORM, hasCapabilities bool
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			parsed, perr := parser.ParseFile(fset, f, nil, 0)
			if perr != nil {
				continue
			}
			for _, decl := range parsed.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				switch fn.Name.Name {
				case "SetRetention":
					if !isUnconditionalUnsupported(fn.Body) {
						implementsRetention = true
					}
				case "Capabilities":
					if !returnsCapabilityLiteral(fn.Body) {
						// A delegating wrapper: it reports whatever the
						// wrapped backend does, so it cannot be wrong.
						continue
					}
					hasCapabilities = true
					if bodyMentionsField(fn.Body, "WORM") {
						declaresWORM = true
					}
				}
			}
		}
		if !hasCapabilities {
			continue // not a storage backend package
		}
		checked++
		if implementsRetention && !declaresWORM {
			t.Errorf("backend %q implements SetRetention but its Capabilities() does not set "+
				"WORM.\n\n    The CAS refuses every chunk when retention is configured and the "+
				"backend reports no WORM, so this backend cannot take a WORM backup at all — "+
				"and the operator is told, wrongly, that it cannot enforce WORM. The remedy "+
				"they would reach for is the flag that turns the guard off.", e.Name())
		}
		if !implementsRetention && declaresWORM {
			t.Errorf("backend %q declares Capabilities().WORM but SetRetention returns "+
				"ErrUnsupported.\n\n    That is the dangerous direction: the gates would pass "+
				"and the operator would believe a freely-deletable backup is immutable.",
				e.Name())
		}
	}
	if checked < 4 {
		t.Fatalf("only %d backend(s) inspected; the guard is not seeing the tree", checked)
	}
}

// returnsCapabilityLiteral reports whether the body states its own
// capabilities as a composite literal, rather than delegating to a
// wrapped plugin.
func returnsCapabilityLiteral(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if _, ok := n.(*ast.CompositeLit); ok {
			found = true
		}
		return true
	})
	return found
}

// isUnconditionalUnsupported reports whether a body is exactly
// `return storage.ErrUnsupported`.
func isUnconditionalUnsupported(body *ast.BlockStmt) bool {
	if len(body.List) != 1 {
		return false
	}
	ret, ok := body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return false
	}
	sel, ok := ret.Results[0].(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "ErrUnsupported"
}

// bodyMentionsField reports whether a composite literal in body sets
// the named field to something other than `false`.
func bodyMentionsField(body *ast.BlockStmt, field string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		id, ok := kv.Key.(*ast.Ident)
		if !ok || id.Name != field {
			return true
		}
		if v, ok := kv.Value.(*ast.Ident); ok && v.Name == "false" {
			return true
		}
		found = true
		return true
	})
	return found
}

func storageRootForGuard(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		candidate := filepath.Join(dir, "internal", "plugin", "storage")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("internal/plugin/storage not found above the working directory")
		}
		dir = parent
	}
}
