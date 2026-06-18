package cmdtree_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
)

// scrubFixture mirrors the prod-shape used by the
// validator + cmdhelp tests: deployment add <name> with
// --connection / --repo, plus a couple of sibling verbs.
// The Scrub tests only need this shape — they don't
// depend on flag types or any other surface area.
//
// (We can't share fixtureRoot from cmdtree_test.go
// because the function lives in another _test.go
// translation unit; copy is cheaper than lifting it to a
// test-helper file.)
func scrubFixture() *cmdtree.Node {
	return cmdtree.Walk(buildSmallTree())
}

func TestScrub_FindsBackingTickedBadCommand(t *testing.T) {
	tree := scrubFixture()
	text := "Run `pg_hardstorage deployment create --name mydb1 --connection postgres://x --repo /tmp/r` to bring it up."
	findings := cmdtree.Scrub(tree, text, "pg_hardstorage")
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1: %v", len(findings), findings)
	}
	if findings[0].Error == nil {
		t.Fatal("finding should have a non-nil Error")
	}
	if findings[0].Error.Kind != "unknown_command" {
		t.Errorf("kind = %q, want unknown_command", findings[0].Error.Kind)
	}
	if !strings.Contains(findings[0].Command, "create") {
		t.Errorf("command should preserve the original text: %q", findings[0].Command)
	}
}

func TestScrub_FindsCommandInFencedCodeBlock(t *testing.T) {
	tree := scrubFixture()
	text := "Try this:\n\n```bash\npg_hardstorage deployment create --name x --connection y --repo z\n```\n\nThen verify."
	findings := cmdtree.Scrub(tree, text, "pg_hardstorage")
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(findings), findings)
	}
	if findings[0].Error == nil {
		t.Fatal("fenced-block finding should be invalid")
	}
}

func TestScrub_HandlesPromptPrefixesInCodeBlock(t *testing.T) {
	tree := scrubFixture()
	// Models often quote shell examples with `$ ` or
	// `> ` — Scrub must look past these.
	text := "```\n$ pg_hardstorage deployment create --name x\n```"
	findings := cmdtree.Scrub(tree, text, "pg_hardstorage")
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(findings), findings)
	}
	if findings[0].Error == nil {
		t.Errorf("$-prefixed bad command should still be flagged")
	}
}

func TestScrub_IgnoresOtherShellCommands(t *testing.T) {
	tree := scrubFixture()
	// We must NOT flag commands that happen to be wrapped
	// in backticks but aren't pg_hardstorage commands.
	text := "First run `psql -c 'SELECT 1'` and then `docker run -d postgres:17`."
	findings := cmdtree.Scrub(tree, text, "pg_hardstorage")
	if len(findings) != 0 {
		t.Errorf("non-pg_hardstorage commands should be ignored; got %+v", findings)
	}
}

func TestScrub_PassesValidCommandSilently(t *testing.T) {
	tree := scrubFixture()
	text := "Add the deployment with `pg_hardstorage deployment add mydb1 --connection postgres://x --repo /tmp/r`."
	findings := cmdtree.Scrub(tree, text, "pg_hardstorage")
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1 (valid)", len(findings))
	}
	if findings[0].Error != nil {
		t.Errorf("valid command should have nil Error; got %+v", findings[0].Error)
	}
}

func TestScrub_DedupesIdenticalCommands(t *testing.T) {
	tree := scrubFixture()
	// Same bad command appears twice — once in prose, once
	// in a code block.  We don't want to double-warn.
	text := "Run `pg_hardstorage deployment create --name x --connection y --repo z`.\n\nThe full form:\n\n```\npg_hardstorage deployment create --name x --connection y --repo z\n```"
	findings := cmdtree.Scrub(tree, text, "pg_hardstorage")
	if len(findings) != 1 {
		t.Errorf("findings = %d, want 1 (deduped)", len(findings))
	}
}

func TestScrub_DoesNotPanicOnMalformedFences(t *testing.T) {
	tree := scrubFixture()
	cases := []string{
		"```\nunclosed fence with `pg_hardstorage deployment create x`",
		"`unclosed single-backtick `pg_hardstorage`",
		"empty: ``",
		"```\n```", // empty fenced block
	}
	for _, c := range cases {
		// Simply asserting no panic is the contract.
		_ = cmdtree.Scrub(tree, c, "pg_hardstorage")
	}
}

func TestScrub_NilTreeReturnsNil(t *testing.T) {
	if findings := cmdtree.Scrub(nil, "`pg_hardstorage anything`", "pg_hardstorage"); findings != nil {
		t.Errorf("nil tree should return nil findings; got %v", findings)
	}
}

func TestScrub_MultipleBadCommands(t *testing.T) {
	tree := scrubFixture()
	text := "Step 1: `pg_hardstorage deployment create x`. Step 2: `pg_hardstorage repo build /tmp`."
	findings := cmdtree.Scrub(tree, text, "pg_hardstorage")
	if len(findings) != 2 {
		t.Fatalf("findings = %d, want 2: %+v", len(findings), findings)
	}
	for _, f := range findings {
		if f.Error == nil {
			t.Errorf("expected both findings to be invalid; got %+v", f)
		}
	}
}

// TestScrub_JoinsBackslashContinuations is the round-6 regression: a
// multi-line command wrapped with trailing `\` must validate as the WHOLE
// command, not its first physical line.  Before the fix, Scrub flagged
// `deployment add mydb \` as missing --connection even though the flag was
// on the next line — a false positive that trained operators to ignore the
// validator. (The live model emitted `repo replicate \` this way.)
func TestScrub_JoinsBackslashContinuations(t *testing.T) {
	tree := scrubFixture()
	text := "Run it:\n\n```bash\n" +
		"pg_hardstorage deployment add mydb \\\n" +
		"  --connection postgres://x \\\n" +
		"  --repo file:///r\n" +
		"```\n"
	findings := cmdtree.Scrub(tree, text, "pg_hardstorage")
	if len(findings) != 1 {
		t.Fatalf("want exactly 1 finding (the joined command), got %d: %+v", len(findings), findings)
	}
	if strings.Contains(findings[0].Command, "\\") {
		t.Errorf("trailing backslash should be gone after the join: %q", findings[0].Command)
	}
	if !strings.Contains(findings[0].Command, "--connection") || !strings.Contains(findings[0].Command, "--repo") {
		t.Errorf("continuation flags should be joined in: %q", findings[0].Command)
	}
	if findings[0].Error != nil {
		t.Errorf("the joined command is valid; should not be flagged: %v", findings[0].Error)
	}
}

// TestScrub_SkipsCommentLines is the round-6 regression: a `#`-led comment
// inside a fenced block that happens to mention the binary must be skipped,
// not un-commented into a pseudo-command.  Before the fix, stripPromptPrefix
// stripped the `# ` and Scrub flagged "unknown subcommand" on the prose.
func TestScrub_SkipsCommentLines(t *testing.T) {
	tree := scrubFixture()
	text := "Note:\n\n```bash\n" +
		"# pg_hardstorage deployment add registers it in the config\n" +
		"pg_hardstorage deployment add mydb --connection postgres://x --repo file:///r\n" +
		"```\n"
	findings := cmdtree.Scrub(tree, text, "pg_hardstorage")
	if len(findings) != 1 {
		t.Fatalf("comment must be skipped — want 1 finding (the real command), got %d: %+v", len(findings), findings)
	}
	if strings.Contains(findings[0].Command, "registers") {
		t.Errorf("comment prose leaked into a command: %q", findings[0].Command)
	}
	if findings[0].Error != nil {
		t.Errorf("the real command is valid; should not be flagged: %v", findings[0].Error)
	}
}
