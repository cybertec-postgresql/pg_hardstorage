package spec_test

// SPEC.md's status column is a product claim, and it cites the package
// that backs each claim. This checks that a package cited by an
// "Implemented" row is actually REACHABLE from a shipped binary.
//
// Why: internal/scim and internal/i18n are both substantial, tested
// implementations — and nothing imports either one. No binary in this
// repository serves a SCIM endpoint or routes a single CLI string
// through i18n.T. SPEC.md nevertheless lists both as Implemented, and
// the v1.0 release notes list them as shipped features. The code exists;
// the product behaviour does not.
//
// SPEC gets this right elsewhere — the cross-account replication ACL row
// says "Planned", and the self-supervised agent paragraph says "Planned,
// not yet implemented — package internal/supervisor/ exists as a
// scaffold". So "Implemented" is a deliberate distinction, which is what
// makes these two look like drift rather than shorthand.
//
// This is the same defect the rest of this audit kept finding — a claim
// nothing checks — one level up, in the document a buyer or auditor
// reads. The two known cases are recorded in the baseline beside this
// file, in the same spirit as test/coverage/deadcorners-baseline.txt:
// they are visible and must be resolved deliberately, and meanwhile no
// NEW unbacked claim can appear unnoticed.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const modulePath = "github.com/cybertec-postgresql/pg_hardstorage/"

// repoRoot is this test's directory's grandparent.
const repoRoot = "../.."

// packageImports maps a module-relative package dir to the
// module-relative packages it imports (non-test files only).
func packageImports(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	fset := token.NewFileSet()
	for _, root := range []string{"internal", "cmd"} {
		base := filepath.Join(repoRoot, root)
		err := filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
			if perr != nil {
				return nil // unparseable (build-tag oddity) — skip, don't fail
			}
			rel, rerr := filepath.Rel(repoRoot, filepath.Dir(p))
			if rerr != nil {
				return nil
			}
			pkg := filepath.ToSlash(rel)
			for _, imp := range f.Imports {
				v := strings.Trim(imp.Path.Value, `"`)
				if strings.HasPrefix(v, modulePath) {
					out[pkg] = append(out[pkg], strings.TrimPrefix(v, modulePath))
				}
			}
			if _, ok := out[pkg]; !ok {
				out[pkg] = nil // record the package even with no local imports
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	if len(out) == 0 {
		t.Fatal("parsed no packages; the guard would pass vacuously")
	}
	return out
}

// reachableFromBinaries returns every module package transitively
// imported by something under cmd/.
func reachableFromBinaries(imports map[string][]string) map[string]bool {
	seen := map[string]bool{}
	var visit func(string)
	visit = func(pkg string) {
		if seen[pkg] {
			return
		}
		seen[pkg] = true
		for _, dep := range imports[pkg] {
			visit(dep)
		}
	}
	for pkg := range imports {
		if strings.HasPrefix(pkg, "cmd/") {
			visit(pkg)
		}
	}
	return seen
}

var (
	// First cell is the feature name (often but not always fully bold —
	// e.g. "| **SCIM 2.0** for user/group provisioning |"), second cell is
	// the status, remainder carries the package citation.
	implementedRow = regexp.MustCompile(`^\|([^|]+)\|\s*Implemented\s*\|(.*)$`)
	citedPackage   = regexp.MustCompile(`internal/[A-Za-z0-9_/]+`)
)

func TestSpec_ImplementedClaimsCiteReachablePackages(t *testing.T) {
	specBody, err := os.ReadFile(filepath.Join(repoRoot, "SPEC.md"))
	if err != nil {
		t.Fatalf("read SPEC.md: %v", err)
	}
	imports := packageImports(t)
	reachable := reachableFromBinaries(imports)

	// Every package that actually exists in the tree.
	exists := map[string]bool{}
	for pkg := range imports {
		exists[pkg] = true
	}

	type claim struct{ feature, pkg string }
	var unbacked []claim
	for _, line := range strings.Split(string(specBody), "\n") {
		m := implementedRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		feature := strings.TrimSpace(strings.ReplaceAll(m[1], "**", ""))
		seenHere := map[string]bool{}
		for _, cited := range citedPackage.FindAllString(m[2], -1) {
			cited = strings.TrimSuffix(cited, "/")
			// Only judge packages that exist as Go packages; SPEC also
			// cites directories that hold no .go files.
			if !exists[cited] || seenHere[cited] {
				continue
			}
			seenHere[cited] = true
			if !reachable[cited] {
				unbacked = append(unbacked, claim{feature, cited})
			}
		}
	}

	baseline := readBaseline(t)
	var fresh []string
	for _, c := range unbacked {
		key := c.pkg
		if baseline[key] {
			delete(baseline, key)
			continue
		}
		fresh = append(fresh, c.pkg+"  (claimed by SPEC row: "+c.feature+")")
	}
	sort.Strings(fresh)

	if len(fresh) > 0 {
		t.Errorf("SPEC.md marks %d feature(s) Implemented whose package no binary can reach:\n  %s\n\n"+
			"Nothing under cmd/ imports these, directly or transitively, so the code cannot run. "+
			"Either wire the package into a binary, or change the SPEC row's status — SPEC already "+
			"uses \"Planned\" where that is the truth (see the cross-account replication ACL row).\n\n"+
			"If the claim is right and this test is wrong, add the package to %s with a comment "+
			"saying why.", len(fresh), strings.Join(fresh, "\n  "), baselinePath)
	}
	// A baseline entry that no longer corresponds to an unbacked claim
	// means someone fixed it; make them delete the line so the file
	// cannot rot into a list of things that stopped being true.
	var stale []string
	for k := range baseline {
		stale = append(stale, k)
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("%s lists %d package(s) that are no longer unbacked claims: %s\n\n"+
			"They are now reachable, or SPEC no longer claims them. Remove the lines.",
			baselinePath, len(stale), strings.Join(stale, ", "))
	}
}

const baselinePath = "test/spec/unwired-claims-baseline.txt"

func readBaseline(t *testing.T) map[string]bool {
	t.Helper()
	body, err := os.ReadFile("unwired-claims-baseline.txt")
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]bool{}
		}
		t.Fatalf("read baseline: %v", err)
	}
	out := map[string]bool{}
	for _, l := range strings.Split(string(body), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out[l] = true
	}
	return out
}
