package cli_test

// docs_jq_paths_test.go — the machine-readable output paths the docs
// hand operators must address the envelope the binary actually emits.
//
// This is the third failure mode of the same family as
// docs_surface_test.go (a knob nobody reads) and docs_flags_test.go (a
// flag that doesn't exist), and it is the quietest of the three.
//
// Nineteen jq expressions across thirteen pages addressed
// `.result.body.<field>`. The envelope has no `body` level:
// output.Result.WithBody assigns to the field tagged `json:"result"`,
// so the body sits directly under `result`. Every one of those
// expressions evaluated to null. The SLO gates in
// docs/operations/slo-as-code.md are `jq -e` — an operator wiring one
// into CI gets a gate keyed on null, and `jq -e` on null exits 1
// forever. The R2 runbook's recovery loop iterates
// `.result.body.deployments[]`, which is "Cannot iterate over null".
//
// `--template` is worse still: text/template renders a missing map key
// as empty and exits 0. `{{.Result.backups}}` — the capitalised root
// this package's own doc comment used to show — prints nothing at all
// and reports success. A monitoring script built on it reports "no
// backups" rather than failing.
//
// Both premises below are derived from the code at run time rather
// than asserted from memory: the test marshals a real Result and reads
// the envelope back. If the envelope ever genuinely grows a `body`
// level, this guard stops flagging by itself instead of becoming a
// lie of its own.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// envelopeShape marshals a real Result and reports the JSON key that
// holds the body, and whether there is a further `body` level beneath
// it.
func envelopeShape(t *testing.T) (root string, nestedBody bool) {
	t.Helper()
	const probe = "__probe__"
	raw, err := json.Marshal(output.NewResult("probe").WithBody(map[string]any{probe: 1}))
	if err != nil {
		t.Fatalf("marshal probe Result: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal probe envelope: %v", err)
	}
	for k, v := range env {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if _, direct := m[probe]; direct {
			_, nestedBody = m["body"]
			return k, nestedBody
		}
		// The body might legitimately sit one level down.
		if inner, ok := m["body"].(map[string]any); ok {
			if _, found := inner[probe]; found {
				return k, true
			}
		}
	}
	t.Fatalf("could not locate the probe body in the envelope: %s", raw)
	return "", false
}

// pathLine is one offending line.
type pathLine struct {
	file string
	line int
	text string
}

// scanLines walks the docs and the Go sources, handing each line to fn.
func scanLines(t *testing.T, root string, fn func(rel string, n int, line string)) {
	t.Helper()
	for _, sub := range []string{"docs", "internal", "cmd"} {
		dir := filepath.Join(root, sub)
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() {
				switch info.Name() {
				case "vendor", ".git", "test-runs", "node_modules":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".md") && !strings.HasSuffix(path, ".go") {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			for i, line := range strings.Split(string(src), "\n") {
				fn(rel, i+1, line)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", sub, err)
		}
	}
}

func report(t *testing.T, bad []pathLine, headline, why string) {
	t.Helper()
	if len(bad) == 0 {
		return
	}
	var b strings.Builder
	for _, v := range bad {
		b.WriteString("\n  " + v.file + ":" + itoa(v.line) + "\n      " + truncURL(strings.TrimSpace(v.text)))
	}
	t.Errorf("%d %s%s\n\n%s", len(bad), headline, b.String(), why)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

// isHistoricalRecord reports whether a file describes what PAST
// versions did rather than what this binary does.
//
// A changelog entry explaining that 19 expressions used to address a
// phantom `.result.body` level has to write that level down to be
// intelligible. The result-path and error-claim guards already exempt
// these paths; this one did not, so documenting the fix broke the test
// that motivated it. The exemption is by path, and it covers the copy
// `make sync-llm-docs` places under internal/llm/docs/root/.
func isHistoricalRecord(rel string) bool {
	switch {
	case strings.HasPrefix(rel, "docs/changelog.md"),
		strings.HasPrefix(rel, "docs/release-notes/"),
		strings.HasSuffix(rel, "llm/docs/root/CHANGELOG.md"),
		rel == "CHANGELOG.md":
		return true
	}
	return false
}

// TestDocumentedJQPathsMatchTheEnvelope catches the phantom `body`
// level.
func TestDocumentedJQPathsMatchTheEnvelope(t *testing.T) {
	root, nested := envelopeShape(t)
	if nested {
		t.Skipf("the envelope really does nest the body under %s.body — "+
			"the documented paths are correct and there is nothing to check", root)
	}

	phantom := regexp.MustCompile(`\.` + regexp.QuoteMeta(root) + `\.body\b`)
	var bad []pathLine
	scanLines(t, repoRootFromTest(t), func(rel string, n int, line string) {
		// This file quotes the wrong form in its own explanation.
		if strings.HasSuffix(rel, "docs_jq_paths_test.go") || isHistoricalRecord(rel) {
			return
		}
		if phantom.MatchString(line) {
			bad = append(bad, pathLine{rel, n, line})
		}
	})

	report(t, bad, "documented output path(s) address a `."+root+".body` level that does not exist:",
		"output.Result.WithBody assigns to the field tagged `json:\""+root+"\"`, so the body sits "+
			"directly under `."+root+"`. A jq expression through the phantom level yields null: "+
			"`jq -e` gates fail forever, `[]` iteration errors, and `--template` renders empty "+
			"with exit 0 — a monitoring script reports \"nothing found\" instead of failing.")
}

// TestDocumentedResultFieldsExistSomewhere is the weakest of the three
// and still the one that catches the most.
//
// Attributing a jq path to the command that produced it, then checking
// the leaf against that command's result struct, needs a per-command
// schema the binary does not expose. What we CAN do without one: take
// the first segment after the envelope root and require it to be a
// JSON tag somewhere in the tree. A name no struct anywhere marshals
// is a name no command can emit.
//
// It is deliberately weak — a field that exists on `list` but not on
// `show` passes — so it produces no false positives from cross-command
// confusion. It still caught `producer_version` ("check what version
// produced a manifest", a capability that does not exist: neither the
// manifest nor the audit event records the producing build) and
// `would_rewrite` (a rotation-progress field the resumability loop
// polled forever).
func TestDocumentedResultFieldsExistSomewhere(t *testing.T) {
	root := repoRootFromTest(t)
	envRoot, _ := envelopeShape(t)

	// Every JSON tag the tree marshals.
	tagRe := regexp.MustCompile("json:\"([a-zA-Z0-9_]+)")
	tags := map[string]bool{}
	scanLines(t, root, func(rel string, _ int, line string) {
		if !strings.HasSuffix(rel, ".go") {
			return
		}
		for _, m := range tagRe.FindAllStringSubmatch(line, -1) {
			tags[m[1]] = true
		}
	})
	if len(tags) == 0 {
		t.Fatal("found no json tags in the tree — the guard would pass vacuously")
	}

	fieldRe := regexp.MustCompile(`\.` + regexp.QuoteMeta(envRoot) + `\.([a-z][a-z0-9_]*)`)
	var bad []pathLine
	seen := map[string]bool{}
	scanLines(t, root, func(rel string, n int, line string) {
		if strings.HasSuffix(rel, "docs_jq_paths_test.go") || isHistoricalRecord(rel) {
			return
		}
		if !strings.Contains(line, "{{") && !strings.Contains(line, "jq ") &&
			!strings.Contains(line, "jq -") && !strings.Contains(line, "'.") {
			return
		}
		for _, m := range fieldRe.FindAllStringSubmatch(line, -1) {
			name := m[1]
			if tags[name] || seen[name] {
				continue
			}
			seen[name] = true
			bad = append(bad, pathLine{rel, n, line})
		}
	})

	report(t, bad, "documented `."+envRoot+".<field>` path(s) name a field no struct in the tree marshals:",
		"the check is intentionally loose — it asks only whether the name exists ANYWHERE, so a "+
			"real field shown on the wrong command still slips through. A name that fails it "+
			"cannot be emitted by any command at all, which means the surrounding prose is "+
			"describing a capability that does not exist.")
}

// TestDocumentedTemplateRootIsTheJSONKey catches the capitalised root.
// Restricted to lines that are template or jq expressions, because Go
// source is full of legitimate `.Result` struct selectors.
func TestDocumentedTemplateRootIsTheJSONKey(t *testing.T) {
	root, _ := envelopeShape(t)
	if root != strings.ToLower(root) {
		t.Skipf("the envelope key %q is not lower-case; this guard assumes it is", root)
	}

	wrongCase := regexp.MustCompile(`\.` + regexp.QuoteMeta(strings.ToUpper(root[:1])+root[1:]) + `\b`)
	var bad []pathLine
	scanLines(t, repoRootFromTest(t), func(rel string, n int, line string) {
		if strings.HasSuffix(rel, "docs_jq_paths_test.go") {
			return
		}
		// Only expressions handed to text/template or jq.
		if !strings.Contains(line, "{{") && !strings.Contains(line, "jq ") {
			return
		}
		if wrongCase.MatchString(line) {
			bad = append(bad, pathLine{rel, n, line})
		}
	})

	report(t, bad, "template/jq expression(s) use a capitalised root:",
		"the template renderer executes against a map produced by a JSON round-trip, so "+
			"text/template's key lookup is case-sensitive against the JSON tag `"+root+"`. "+
			"A capitalised root matches no key, and text/template renders a missing key as "+
			"the empty string with exit 0 — the failure is silent.")
}
