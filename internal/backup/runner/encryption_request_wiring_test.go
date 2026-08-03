package runner_test

// encryption_request_wiring_test.go — every caller that resolves a
// backup's encryption posture must supply BOTH the KEK reference and
// its provider configuration.
//
// This is the write-side twin of
// internal/cli/kms_provider_wiring_test.go. The read side (unwrap) has
// already produced this bug four times; the write side has the same
// shape and the same blast radius, and currently three near-identical
// EncryptionRequest literals are maintained by hand in three packages:
//
//	internal/cli/backup.go     the interactive CLI
//	internal/cli/agent.go      the local schedule engine
//	internal/agent/executor.go the control-plane executor
//
// Omitting KEKRef in any of them silently downgrades that path to the
// local keyring — which is the original issue #44 defect: scheduled
// backups wrapped under kek.bin while interactive ones used the cloud
// KEK, and plaintext-hash dedup then welded both postures onto the same
// chunks. Omitting KMSConfig is the subtler half: it works against any
// provider needing no configuration and fails only for deployments that
// declare a region or credential.
//
// Neither omission is visible in a test that does not configure a
// provider, so this asserts the structure directly. `runner` owns the
// type, so the invariant lives with it rather than with any one caller.
//
// A caller that legitimately cannot supply one (a genuinely
// keyring-only path) belongs in allowedPartialRequests with a reason.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// allowedPartialRequests maps "pkgdir/file.go:FuncName" to why that
// site may omit a field. Empty is the goal.
var allowedPartialRequests = map[string]string{}

// requiredRequestFields are the two that carry the operator's intent.
// Encrypt/NoEncrypt are genuinely optional — non-interactive callers
// leave both false to get the auto-detect posture.
var requiredRequestFields = []string{"KEKRef", "KMSConfig"}

func internalRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/backup/runner → internal
	return filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
}

func TestEncryptionRequestAlwaysCarriesKEKAndProviderConfig(t *testing.T) {
	root := internalRoot(t)

	var dirs []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	fset := token.NewFileSet()
	found := 0

	for _, dir := range dirs {
		pkgs, perr := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
			return !strings.HasSuffix(fi.Name(), "_test.go")
		}, 0)
		if perr != nil {
			continue // not a Go package dir, or unparseable — other tests cover that
		}
		for _, pkg := range pkgs {
			for path, file := range pkg.Files {
				rel, _ := filepath.Rel(root, path)
				rel = filepath.ToSlash(rel)

				type span struct {
					name       string
					start, end token.Pos
				}
				var fns []span
				for _, d := range file.Decls {
					if fd, ok := d.(*ast.FuncDecl); ok {
						fns = append(fns, span{fd.Name.Name, fd.Pos(), fd.End()})
					}
				}
				enclosing := func(p token.Pos) string {
					for _, f := range fns {
						if p >= f.start && p <= f.end {
							return f.name
						}
					}
					return "<top-level>"
				}

				ast.Inspect(file, func(n ast.Node) bool {
					lit, ok := n.(*ast.CompositeLit)
					if !ok {
						return true
					}
					// Matches both runner.EncryptionRequest{...} from
					// another package and EncryptionRequest{...} inside
					// package runner itself.
					name := ""
					switch tt := lit.Type.(type) {
					case *ast.SelectorExpr:
						name = tt.Sel.Name
					case *ast.Ident:
						name = tt.Name
					}
					if name != "EncryptionRequest" {
						return true
					}
					found++

					set := map[string]bool{}
					for _, el := range lit.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok {
								set[k.Name] = true
							}
						}
					}

					fn := enclosing(lit.Pos())
					key := rel + ":" + fn
					if reason, exempt := allowedPartialRequests[key]; exempt {
						t.Logf("%s exempt: %s", key, reason)
						return true
					}
					for _, want := range requiredRequestFields {
						if set[want] {
							continue
						}
						pos := fset.Position(lit.Pos())
						t.Errorf("%s:%d: EncryptionRequest in %s() omits %s — "+
							"a path that drops KEKRef silently falls back to the local keyring "+
							"(the issue #44 defect: scheduled and interactive backups wrapping "+
							"under different KEKs onto shared chunks); a path that drops "+
							"KMSConfig fails only for deployments declaring a region or "+
							"credential. Supply it, or add %q to allowedPartialRequests.",
							rel, pos.Line, fn, want, key)
					}
					return true
				})
			}
		}
	}

	if found == 0 {
		t.Fatal("no EncryptionRequest literals found — the scan stopped matching, " +
			"so this test is no longer checking anything")
	}
	t.Logf("checked %d EncryptionRequest construction site(s)", found)
}
