package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
)

// TestDocumentedCommandsAreValid runs every `pg_hardstorage ...` command
// shown in the docs through the same validator the LLM helper uses
// against the live CLI tree.
//
// The docs are what operators copy under pressure, and several of this
// release's bugs were the docs asserting something the product does not
// do. The operator guide's restore section documented an interactive
// `pg_hardstorage restore db1` that "lists backups, prompts for selection
// and asks for confirmation". restore takes exactly two arguments; that
// command is a usage error. Nothing checked it.
//
// Extraction reads blocks the way a shell user would: every line of a
// sh / bash / shell fence, and only `$ `-prefixed lines of a console
// fence (the rest is output — `pg_hardstorage v1.0.x (abcdef1, ...)`
// is what `version` prints, not a command). Generated CLI reference
// pages are excluded; they are regenerated from the tree itself.
func TestDocumentedCommandsAreValid(t *testing.T) {
	tree := cmdtree.Walk(NewRoot())
	fence := regexp.MustCompile("(?s)```(sh|bash|shell|console)\n(.*?)```")

	var checked int
	err := filepath.WalkDir("../../docs", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") ||
			strings.Contains(path, "/reference/cli/") {
			return err
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(raw)
		for _, m := range fence.FindAllStringSubmatchIndex(src, -1) {
			lang := src[m[2]:m[3]]
			body := strings.ReplaceAll(src[m[4]:m[5]], "\\\n", " ")
			// A sh/bash block that shows `$ ` prompts is a transcript:
			// its unprefixed lines are output, exactly as in console.
			if lang != "console" && regexp.MustCompile(`(?m)^\s*\$ `).MatchString(body) {
				lang = "console"
			}
			startLine := strings.Count(src[:m[4]], "\n") + 1
			for i, line := range strings.Split(body, "\n") {
				l := strings.TrimSpace(line)
				if lang == "console" {
					if !strings.HasPrefix(l, "$ ") {
						continue
					}
					l = strings.TrimSpace(strings.TrimPrefix(l, "$ "))
				} else {
					l = strings.TrimSpace(strings.TrimPrefix(l, "$ "))
				}
				if !strings.HasPrefix(l, "pg_hardstorage ") {
					continue
				}
				checked++
				if verr := cmdtree.Validate(tree, l, "pg_hardstorage"); verr != nil {
					t.Errorf("%s:%d\n    %s\n    %v", path, startLine+i, l, verr)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 100 {
		t.Fatalf("only %d documented commands found — extraction is broken, not the docs", checked)
	}
	t.Logf("validated %d documented commands", checked)
}
