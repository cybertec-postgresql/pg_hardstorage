package regression

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoDocumentedCodeHidesInMessageText guards the class of bug that hid
// split-brain: a documented error code written as a MESSAGE PREFIX
// (fmt.Errorf("splitbrain.content_mismatch: ...")) instead of set as the
// structured code. The CLI wrapped it as wal.push_failed, and automation
// routing on the documented `splitbrain.*` never matched.
//
// The splitbrain prefixes are allowed because the CLI now lifts them
// into the code (splitBrainCode in internal/cli/wal.go, with end-to-end
// tests). Any OTHER documented code or namespace appearing as a message
// prefix must either be emitted as an output.Error code or be added here
// together with a lift and a test.
func TestNoDocumentedCodeHidesInMessageText(t *testing.T) {
	root := repoRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "docs/reference/error-codes.md"))
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile("`([a-z_]+\\.[a-z0-9_]+)`").FindAllStringSubmatch(string(doc), -1) {
		documented[m[1]] = true
	}
	namespaces := map[string]bool{}
	for _, m := range regexp.MustCompile("`([a-z_]+)\\.\\*`").FindAllStringSubmatch(string(doc), -1) {
		namespaces[m[1]] = true
	}
	lifted := map[string]bool{"splitbrain": true}
	prefix := regexp.MustCompile(`(?:Errorf|errors\.New)\(\s*"([a-z_]+)\.([a-z0-9_]+):`)

	for _, dir := range []string{"internal", "compat", "cmd"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") ||
				strings.HasSuffix(p, "_test.go") || strings.Contains(p, "_mutation_") {
				return err
			}
			f, oerr := os.Open(p)
			if oerr != nil {
				return oerr
			}
			defer f.Close()
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			for n := 1; sc.Scan(); n++ {
				for _, m := range prefix.FindAllStringSubmatch(sc.Text(), -1) {
					code := m[1] + "." + m[2]
					if (documented[code] || namespaces[m[1]]) && !lifted[m[1]] {
						rel, _ := filepath.Rel(root, p)
						t.Errorf("%s:%d: documented code %q is only a message prefix — "+
							"emit it as an output.Error code, or lift it like splitBrainCode", rel, n, code)
					}
				}
			}
			return nil
		})
	}
}
