// ssh_credential_symmetry_test.go — scp and sftp must expose the SAME
// credential surface.
//
// The behavioural tests next door can only reach known_hosts and
// identity_file: those two are resolved into a file the plugin opens,
// so the error names the path and precedence is observable without a
// server. password and identity_passphrase are handed straight to the
// SSH handshake, so proving anything about them behaviourally needs a
// live sshd for every case — and the interesting failures here are not
// about handshakes at all. They are "this knob was added to one plugin
// and not the other" and "the env var is spelled wrong".
//
// Reading the source answers both directly. This mirrors the AST
// meta-tests already used for CLI wiring and error-code truthfulness.
//
// What it pins:
//
//   - both plugins resolve exactly the same set of settings;
//   - each reads Extras FIRST, then the environment;
//   - the env var is PG_HARDSTORAGE_<PLUGIN>_<SETTING>, so scp's docs
//     and sftp's cannot drift into naming different variables for the
//     same thing.

package storage_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// credentialWiring maps an Extras key to the env var consulted after it.
type credentialWiring map[string]string

// parseCredentialWiring extracts every
//
//	firstNonEmpty(cfg.Extras["k"], os.Getenv("V"))
//
// from a plugin source file. The two-argument shape IS the contract:
// Extras first, environment second.
func parseCredentialWiring(t *testing.T, file string) credentialWiring {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	got := credentialWiring{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok || fn.Name != "firstNonEmpty" || len(call.Args) != 2 {
			return true
		}
		key, ok := extrasKey(call.Args[0])
		if !ok {
			// Argument order reversed, or a source other than Extras
			// first. Report rather than skip: a silent skip here would
			// make the whole file look correctly wired.
			t.Errorf("%s: firstNonEmpty's first argument is not cfg.Extras[...]; Extras "+
				"must be consulted before the environment", filepath.Base(file))
			return true
		}
		env, ok := getenvName(call.Args[1])
		if !ok {
			t.Errorf("%s: firstNonEmpty(cfg.Extras[%q], …) second argument is not "+
				"os.Getenv(\"…\")", filepath.Base(file), key)
			return true
		}
		got[key] = env
		return true
	})
	return got
}

// extrasKey matches cfg.Extras["<key>"].
func extrasKey(e ast.Expr) (string, bool) {
	ix, ok := e.(*ast.IndexExpr)
	if !ok {
		return "", false
	}
	sel, ok := ix.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Extras" {
		return "", false
	}
	return stringLit(ix.Index)
}

// getenvName matches os.Getenv("<name>").
func getenvName(e ast.Expr) (string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Getenv" {
		return "", false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "os" {
		return "", false
	}
	return stringLit(call.Args[0])
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", ".."))
}

// TestSSHCredentials_PluginsShareOneSurface is the symmetry check.
func TestSSHCredentials_PluginsShareOneSurface(t *testing.T) {
	root := repoRoot(t)
	scpW := parseCredentialWiring(t, filepath.Join(root, "internal", "plugin", "storage", "scp", "scp.go"))
	sftpW := parseCredentialWiring(t, filepath.Join(root, "internal", "plugin", "storage", "sftp", "sftp.go"))

	if len(scpW) == 0 || len(sftpW) == 0 {
		t.Fatalf("found no credential wiring (scp=%d sftp=%d) — the firstNonEmpty shape "+
			"this test recognises has changed, so it is now asserting nothing",
			len(scpW), len(sftpW))
	}

	// 1. The same settings, on both.
	if diff := keyDiff(scpW, sftpW); diff != "" {
		t.Errorf("the two SSH plugins no longer resolve the same settings:\n%s\n"+
			"A credential added to one and not the other is invisible until an operator "+
			"configures the backend that lacks it", diff)
	}

	// 2. Env vars follow PG_HARDSTORAGE_<PLUGIN>_<SETTING>.
	for _, p := range []struct {
		prefix string
		wiring credentialWiring
	}{
		{"PG_HARDSTORAGE_SCP", scpW},
		{"PG_HARDSTORAGE_SFTP", sftpW},
	} {
		for key, env := range p.wiring {
			want := p.prefix + "_" + strings.ToUpper(key)
			if env != want {
				t.Errorf("%s: extras %q reads %s, want %s — the docs derive the variable "+
					"name from the setting, so an off-pattern name is one an operator will "+
					"set correctly and see ignored", p.prefix, key, env, want)
			}
		}
	}
}

// TestSSHCredentials_KnownSettingsAreWired guards against the whole
// mechanism being renamed out from under the symmetry check: if
// firstNonEmpty were replaced wholesale, parseCredentialWiring would
// find nothing and every comparison above would trivially hold.
func TestSSHCredentials_KnownSettingsAreWired(t *testing.T) {
	root := repoRoot(t)
	want := []string{"identity_file", "identity_passphrase", "known_hosts", "password"}

	for _, p := range []struct{ name, file string }{
		{"scp", filepath.Join(root, "internal", "plugin", "storage", "scp", "scp.go")},
		{"sftp", filepath.Join(root, "internal", "plugin", "storage", "sftp", "sftp.go")},
	} {
		t.Run(p.name, func(t *testing.T) {
			w := parseCredentialWiring(t, p.file)
			var got []string
			for k := range w {
				got = append(got, k)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("settings = %v, want %v", got, want)
			}
		})
	}
}

func keyDiff(a, b credentialWiring) string {
	var out []string
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, "  only in scp:  "+k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, "  only in sftp: "+k)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}
