package cli_test

// kms_provider_wiring_test.go — every DEK-unwrap site must resolve its
// provider configuration.
//
// `keystore.UnwrapOpts.ProviderConfig` carries the `kms.providers`
// settings (region, endpoint, credentials) for a KEKRef. A call site
// that omits it works fine against any provider that needs no
// configuration — a local keyring, or a cloud provider running on
// ambient credentials — and fails ONLY for the deployments that supply
// an explicit region or credential. That is the worst shape of bug:
// invisible in every test that doesn't happen to configure a provider,
// and total for the operators who do.
//
// It has now happened four times in this codebase:
//
//   - recovery drills hardcoded a nil provider config, so a drill of a
//     cloud-KMS deployment could never pass — and `doctor` then reported
//     its backups as unproven (CRITICAL), a red light manufactured
//     entirely by the tooling.
//   - `partial dump` omitted it.
//   - `decryptingCASFromEnvelope` in wal.go passed a completely empty
//     UnwrapOpts on its cloud branch, and reports failure as ok=false,
//     so the operator saw an unrelated downstream error instead.
//   - `resolveDEKForVerify` — used by `verify --full`, standby create
//     and timetravel restore — omitted it while its own doc comment
//     called it "the package-shared cloud-capable DEK resolver".
//
// A flag-based test would not have caught any of them: drills, standby
// create and timetravel restore expose no `--kms-config` flag at all.
// The invariant is not about flags, it is about the call site. So this
// walks the package's syntax tree and requires every UnwrapOpts literal
// to set ProviderConfig.
//
// If a future call site genuinely cannot resolve config, add it to
// allowedWithoutProviderConfig with the reason — an explicit, reviewed
// exemption rather than a silent omission.

import (
	"io/fs"

	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// allowedWithoutProviderConfig maps "file.go:FuncName" to why that site
// legitimately omits ProviderConfig. Keep it empty if you can.
var allowedWithoutProviderConfig = map[string]string{}

func cliPackageDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(here)
}

func TestUnwrapOptsAlwaysResolvesProviderConfig(t *testing.T) {
	dir := cliPackageDir(t)
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}

	found := 0
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			base := filepath.Base(path)

			// Track the enclosing function so a failure names it.
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
				sel, ok := lit.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "UnwrapOpts" {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "keystore" {
					return true
				}
				found++

				hasProviderConfig := false
				for _, el := range lit.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "ProviderConfig" {
						hasProviderConfig = true
					}
				}
				if hasProviderConfig {
					return true
				}

				fn := enclosing(lit.Pos())
				key := base + ":" + fn
				if reason, exempt := allowedWithoutProviderConfig[key]; exempt {
					t.Logf("%s exempt: %s", key, reason)
					return true
				}
				pos := fset.Position(lit.Pos())
				t.Errorf("%s:%d: keystore.UnwrapOpts in %s() omits ProviderConfig — "+
					"a deployment whose kms.providers entry supplies a region or credential "+
					"cannot unwrap here, and this will pass every test that does not configure "+
					"a provider. Resolve it (deploymentKMSResolver) or add %q to "+
					"allowedWithoutProviderConfig with a reason.",
					base, pos.Line, fn, key)
				return true
			})
		}
	}

	if found == 0 {
		t.Fatal("no keystore.UnwrapOpts literals found — the scan stopped matching, " +
			"so this test is no longer checking anything")
	}
	t.Logf("checked %d keystore.UnwrapOpts construction site(s)", found)
}
