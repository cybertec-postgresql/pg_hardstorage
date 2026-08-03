package cli_test

// release_workflow_test.go — meta-tests over the release pipeline
// itself.
//
// The last release cycle lost three things to CI wiring rather than to
// code: v1.0.16, v1.0.17 and v1.1.0 all shipped without SLSA
// provenance because a Homebrew cask-push failure marked goreleaser
// failed AFTER every artifact was already published, and the
// downstream provenance job was skipped with it. Separately, the vuln
// gate had been failing OPEN for months because its only mode panics
// on Go 1.26.
//
// None of that is reachable by testing Go code. These tests read the
// workflow and the Makefile as data and assert the properties that
// were violated.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func repoRootFromCLI(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
}

func readRepoFile(t *testing.T, root string, parts ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{root}, parts...)...)
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(parts...), err)
	}
	return string(body)
}

// --- #16: goreleaser's env references must be wired ---------------

// TestGoreleaserEnvRefsAreProvided fails when .goreleaser.yaml reads an
// environment variable the release workflow never sets. goreleaser
// resolves `{{ .Env.X }}` at template time and errors on an UNSET
// variable — mid-release, after builds have started.
//
// HONEST SCOPE: this checks WIRING, not VALUES. It would NOT have
// caught the HOMEBREW_TAP_TOKEN failure that has broken the cask on
// every release — that variable is correctly wired to
// `secrets.HOMEBREW_TAP_TOKEN`; the secret itself is unset or
// unscoped, so it expands to an empty string and GitHub answers 401.
// A static file cannot see that. The runtime half is the preflight
// step asserted by TestReleaseWorkflowPreflightsRequiredSecrets below.
func TestGoreleaserEnvRefsAreProvided(t *testing.T) {
	root := repoRootFromCLI(t)
	goreleaser := readRepoFile(t, root, ".goreleaser.yaml")
	workflow := readRepoFile(t, root, ".github", "workflows", "release.yml")

	refs := map[string]bool{}
	for _, m := range regexp.MustCompile(`\{\{ *\.Env\.([A-Z_][A-Z0-9_]*) *\}\}`).
		FindAllStringSubmatch(goreleaser, -1) {
		refs[m[1]] = true
	}
	if len(refs) == 0 {
		t.Fatal("no {{ .Env.X }} references found in .goreleaser.yaml — " +
			"the scan stopped matching and this test checks nothing")
	}

	provided := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*([A-Z_][A-Z0-9_]*):\s*\$\{\{`).
		FindAllStringSubmatch(workflow, -1) {
		provided[m[1]] = true
	}

	for _, name := range sortedKeysOf(refs) {
		if !provided[name] {
			t.Errorf(".goreleaser.yaml reads {{ .Env.%s }} but release.yml never sets %s — "+
				"goreleaser errors on an unset variable, mid-release, after the builds "+
				"have already run", name, name)
		}
	}
}

func sortedKeysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- #15: artifact guarantees must not hang off unrelated steps ----

// artifactGuaranteeJobs are jobs that deliver a guarantee ABOUT the
// published artifacts, and therefore must still run when an earlier
// job fails for a reason that does not affect those artifacts.
//
// goreleaser publishes the GitHub Release and every asset BEFORE its
// Homebrew cask push. When that push 401s, the job is marked failed —
// with the artifacts already live. Under the default on-success
// condition, everything downstream is skipped, so a CASK PUBLISHING
// problem silently cost three releases their supply-chain attestation
// while docs/compliance/slsa-l3-provenance.md told operators how to
// verify one.
//
// The fix is `if: ${{ !cancelled() && … }}`, with the real guard being
// a non-empty checksum output — if goreleaser died BEFORE building,
// there is nothing to attest and the job correctly does not run.
var artifactGuaranteeJobs = map[string]string{
	"slsa-provenance": "supply-chain attestation for artifacts goreleaser has already published",
	"homebrew-smoke":  "verifies the published release installs from the tap",
}

type workflowFile struct {
	Jobs map[string]struct {
		Needs any    `yaml:"needs"`
		If    string `yaml:"if"`
	} `yaml:"jobs"`
}

func TestArtifactJobsSurviveUnrelatedFailures(t *testing.T) {
	root := repoRootFromCLI(t)
	var wf workflowFile
	if err := yaml.Unmarshal([]byte(readRepoFile(t, root, ".github", "workflows", "release.yml")), &wf); err != nil {
		t.Fatalf("parse release.yml: %v", err)
	}
	if len(wf.Jobs) == 0 {
		t.Fatal("parsed no jobs from release.yml")
	}

	for name, why := range artifactGuaranteeJobs {
		job, ok := wf.Jobs[name]
		if !ok {
			t.Errorf("release.yml has no job %q — either it was renamed (update this "+
				"test) or removed (the guarantee it delivered is gone: %s)", name, why)
			continue
		}
		if job.Needs == nil {
			continue // nothing upstream can suppress it
		}
		if !strings.Contains(job.If, "cancelled()") {
			t.Errorf("job %q declares `needs` but its `if` (%q) does not reference "+
				"cancelled() — under the default on-success condition an upstream "+
				"failure that does NOT affect the artifacts will skip it. This job %s. "+
				"Use `if: ${{ !cancelled() && <real guard> }}`.", name, job.If, why)
		}
	}
}

// TestSLSAJobGuardsOnArtifactsNotJobStatus pins the SECOND half of that
// fix. `!cancelled()` alone would run the provenance generator even
// when goreleaser died before producing anything; the meaningful guard
// is that the checksum output is non-empty.
func TestSLSAJobGuardsOnArtifactsNotJobStatus(t *testing.T) {
	root := repoRootFromCLI(t)
	var wf workflowFile
	if err := yaml.Unmarshal([]byte(readRepoFile(t, root, ".github", "workflows", "release.yml")), &wf); err != nil {
		t.Fatalf("parse release.yml: %v", err)
	}
	job, ok := wf.Jobs["slsa-provenance"]
	if !ok {
		t.Fatal("release.yml has no slsa-provenance job")
	}
	if !strings.Contains(job.If, "subject-checksums") {
		t.Errorf("slsa-provenance `if` (%q) does not test the checksum output — "+
			"without that guard the generator runs even when goreleaser built nothing, "+
			"and attests an empty subject set", job.If)
	}
}

// TestReleaseWorkflowPreflightsRequiredSecrets is the runtime half the
// static wiring check cannot cover.
//
// HOMEBREW_TAP_TOKEN is correctly wired and still fails, because the
// secret resolves to an empty string. goreleaser then spends about six
// minutes building and publishing everything before dying at the very
// last step on a 401. A preflight that refuses in seconds turns a
// six-minute-deep failure into an immediate, unambiguous one.
func TestReleaseWorkflowPreflightsRequiredSecrets(t *testing.T) {
	root := repoRootFromCLI(t)
	workflow := readRepoFile(t, root, ".github", "workflows", "release.yml")
	if !strings.Contains(workflow, "preflight-secrets") {
		t.Error("release.yml has no preflight-secrets step — a secret that is wired but " +
			"EMPTY (the standing HOMEBREW_TAP_TOKEN failure) is invisible to any static " +
			"check and currently surfaces only after goreleaser has published everything")
	}
}

// --- #14: the vulnerability gate must be able to fail --------------

// TestGovulncheckGateFailsClosed asserts the STRUCTURE that makes the
// gate real.
//
// It ran only govulncheck's source mode, which on Go 1.26 panics with
// "ForEachElement called on type containing *types.TypeParam" — a bug
// in govulncheck's own x/tools dependency. A gate that panics reports
// nothing, and nothing reads as clean. It had been failing OPEN, and
// three CVEs reached a release candidate through it.
//
// Binary mode does no call-graph analysis, so it survives that skew and
// is the hard gate. What must hold:
//
//   - binary mode is invoked at all;
//   - its line is NOT prefixed with `-` (make's ignore-errors marker),
//     which is what source mode uses and is exactly how a gate goes
//     quiet again;
//   - it scans an UNSTRIPPED build, because `-ldflags -s -w` removes
//     the symbol table govulncheck needs and degrades it to module
//     granularity, flagging dependencies nothing calls.
//
// Synthesising a known-vulnerable binary would test the tool rather
// than the gate, and would rot as advisories are published; the
// structural properties are what actually regressed.
func TestGovulncheckGateFailsClosed(t *testing.T) {
	root := repoRootFromCLI(t)
	mk := readRepoFile(t, root, "Makefile")

	recipe := makeRecipe(t, mk, "govulncheck")
	if len(recipe) == 0 {
		t.Fatal("no govulncheck target found in the Makefile")
	}

	var binaryLine string
	for _, ln := range recipe {
		if strings.Contains(ln, "-mode=binary") {
			binaryLine = ln
			break
		}
	}
	if binaryLine == "" {
		t.Fatal("the govulncheck target never runs `-mode=binary` — source mode alone " +
			"panics on Go 1.26 and reports nothing, which is how this gate previously " +
			"failed open while three CVEs shipped")
	}
	if strings.HasPrefix(strings.TrimSpace(binaryLine), "-") {
		t.Error("the binary-mode govulncheck line is prefixed with `-`, so make IGNORES " +
			"its exit status — the gate cannot fail and is decorative")
	}
	if !strings.Contains(strings.Join(recipe, "\n"), ".vulnscan") {
		t.Error("the gate does not build a separate unstripped binary to scan — the " +
			"release build strips symbols with `-ldflags -s -w`, and without them " +
			"govulncheck cannot do reachability analysis and reports every linked " +
			"module instead, including ones with no fix available")
	}
}

// makeRecipe returns the tab-indented body of a Makefile target.
func makeRecipe(t *testing.T, mk, target string) []string {
	t.Helper()
	var out []string
	in := false
	for _, ln := range strings.Split(mk, "\n") {
		if strings.HasPrefix(ln, target+":") {
			in = true
			continue
		}
		if in {
			if !strings.HasPrefix(ln, "\t") {
				break
			}
			out = append(out, strings.TrimPrefix(ln, "\t"))
		}
	}
	return out
}
