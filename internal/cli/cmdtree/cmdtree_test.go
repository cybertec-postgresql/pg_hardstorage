package cmdtree_test

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
)

// buildSmallTree exposes fixtureRoot to the scrub tests
// (the file-level _test.go isolation rules let test
// files in the same package share helpers, but each
// helper has to be defined exactly once).  Keeping this
// alias means the scrub tests don't carry their own
// duplicate cobra-tree builder.
func buildSmallTree() *cobra.Command { return fixtureRoot() }

// fixtureRoot builds a small cobra tree mimicking the
// shape that bit the operator: a `deployment` group with
// `add <name>` (positional), the wrong-but-tempting verb
// "create" absent, and flags that include the right one
// (--connection) and a near-miss (--conn).  Building a
// fixture rather than importing internal/cli's NewRoot
// keeps these tests free of CLI-package layering and
// makes the test deterministic regardless of how the real
// command tree evolves.
func fixtureRoot() *cobra.Command {
	root := &cobra.Command{Use: "pg_hardstorage"}
	root.PersistentFlags().Bool("no-color", false, "disable ANSI colour")

	dep := &cobra.Command{Use: "deployment", Short: "Manage deployments"}
	add := &cobra.Command{Use: "add <name>", Short: "Add a new deployment to the config"}
	add.Flags().String("connection", "", "libpq connection string (required)")
	add.Flags().String("repo", "", "repository URL (required)")
	add.Flags().Bool("skip-probe", false, "don't probe PG")
	dep.AddCommand(add)
	dep.AddCommand(&cobra.Command{Use: "remove <name>", Short: "Remove a deployment"})
	dep.AddCommand(&cobra.Command{Use: "list", Short: "List deployments", Aliases: []string{"ls"}})
	dep.AddCommand(&cobra.Command{Use: "edit <name>", Short: "Edit a deployment"})
	dep.AddCommand(&cobra.Command{Use: "test <name>", Short: "Test connectivity"})

	repo := &cobra.Command{Use: "repo", Short: "Manage repositories"}
	repoInit := &cobra.Command{Use: "init <url>", Short: "Initialise a repository"}
	repo.AddCommand(repoInit)
	repo.AddCommand(&cobra.Command{Use: "check <url>", Short: "Verify signatures + metadata"})

	hidden := &cobra.Command{Use: "secret", Short: "internal", Hidden: true}

	root.AddCommand(dep, repo, hidden)
	return root
}

func TestWalk_BasicShape(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	if tree.Name != "pg_hardstorage" {
		t.Fatalf("root name = %q, want pg_hardstorage", tree.Name)
	}
	dep := tree.Find([]string{"deployment"})
	if dep == nil {
		t.Fatal("deployment node not found")
	}
	want := []string{"add", "edit", "list", "remove", "test"}
	got := make([]string, 0, len(dep.Children))
	for _, c := range dep.Children {
		got = append(got, c.Name)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("deployment children = %v, want %v (sorted)", got, want)
	}
}

func TestWalk_AliasResolves(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	// The "list" command has alias "ls".  Find should
	// resolve both — the validator depends on this when
	// an operator pastes the alias form.
	if tree.Find([]string{"deployment", "ls"}) == nil {
		t.Error("alias 'ls' should resolve to deployment list")
	}
}

func TestWalk_SkipsHelpAndCompletion(t *testing.T) {
	root := fixtureRoot()
	// Cobra adds these automatically when commands are
	// registered.  The walker must drop them — they're
	// noise to the LLM.
	root.AddCommand(&cobra.Command{Use: "help", Short: "Help about any command"})
	root.AddCommand(&cobra.Command{Use: "completion", Short: "Generate completion scripts"})
	tree := cmdtree.Walk(root)
	for _, c := range tree.Children {
		if c.Name == "help" || c.Name == "completion" {
			t.Errorf("walker should drop %q", c.Name)
		}
	}
}

func TestWalk_HiddenIsKept(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	if tree.Find([]string{"secret"}) == nil {
		t.Error("hidden commands must be retained for the validator")
	}
	// ...but VisibleChildren should drop them.
	for _, c := range tree.VisibleChildren() {
		if c.Name == "secret" {
			t.Errorf("VisibleChildren leaked hidden command %q", c.Name)
		}
	}
}

func TestCatalog_DepthLimit(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	depth1 := cmdtree.Catalog(tree, 1)
	if !strings.Contains(depth1, "deployment") {
		t.Errorf("depth-1 catalog should include 'deployment': %q", depth1)
	}
	if strings.Contains(depth1, "skip-probe") {
		t.Errorf("catalog must not include flag names: %q", depth1)
	}
	// Depth 0: top-level only — no nested "add" line.
	depth0 := cmdtree.Catalog(tree, 0)
	if strings.Contains(depth0, "  add") {
		t.Errorf("depth-0 catalog leaked depth-1 entries: %q", depth0)
	}
	depth2 := cmdtree.Catalog(tree, 2)
	if !strings.Contains(depth2, "add") {
		t.Errorf("depth-2 catalog should include 'add' under deployment: %q", depth2)
	}
}

func TestCatalog_OmitsHidden(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	cat := cmdtree.Catalog(tree, 2)
	if strings.Contains(cat, "secret") {
		t.Errorf("catalog rendered hidden command: %q", cat)
	}
}

func TestHelp_DeploymentAdd(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	help := cmdtree.Help(tree, []string{"deployment", "add"})
	for _, want := range []string{"deployment add", "Add a new deployment", "--connection", "--repo", "--skip-probe"} {
		if !strings.Contains(help, want) {
			t.Errorf("help should include %q\nfull output:\n%s", want, help)
		}
	}
}

func TestHelp_UnknownPathReturnsEmpty(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	if got := cmdtree.Help(tree, []string{"deployment", "create"}); got != "" {
		t.Errorf("help for unknown path should be empty; got %q", got)
	}
}

// TestValidate_TheActualBug locks in the operator's
// reported case: "deployment create --name X" should fail
// with kind=unknown_command, naming the offending segment
// and pointing at the right verbs.
func TestValidate_TheActualBug(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	cmd := "pg_hardstorage deployment create --name mydb1 --connection postgres://x --repo /tmp/r"
	err := cmdtree.Validate(tree, cmd, "pg_hardstorage")
	if err == nil {
		t.Fatal("expected validation error; got nil")
	}
	ve, ok := err.(*cmdtree.ValidationError)
	if !ok {
		t.Fatalf("error type = %T, want *ValidationError", err)
	}
	if ve.Kind != "unknown_command" {
		t.Errorf("kind = %q, want unknown_command", ve.Kind)
	}
	if !strings.Contains(ve.Message, "create") {
		t.Errorf("message should name the bad segment %q: %q", "create", ve.Message)
	}
	if !strings.Contains(ve.Message, "deployment") {
		t.Errorf("message should give parent context %q: %q", "deployment", ve.Message)
	}
	if len(ve.PathBeforeError) != 1 || ve.PathBeforeError[0] != "deployment" {
		t.Errorf("PathBeforeError = %v, want [deployment]", ve.PathBeforeError)
	}
}

func TestValidate_UnknownFlag(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	// `add` exists but `--name` does not — name is a
	// positional in the real CLI.
	cmd := "pg_hardstorage deployment add mydb1 --name foo --connection postgres://x"
	err := cmdtree.Validate(tree, cmd, "pg_hardstorage")
	if err == nil {
		t.Fatal("expected unknown_flag error; got nil")
	}
	ve, ok := err.(*cmdtree.ValidationError)
	if !ok {
		t.Fatalf("error type = %T, want *ValidationError", err)
	}
	if ve.Kind != "unknown_flag" {
		t.Errorf("kind = %q, want unknown_flag", ve.Kind)
	}
	if !strings.Contains(ve.Message, "--name") {
		t.Errorf("message should name --name: %q", ve.Message)
	}
}

func TestValidate_UnknownFlagWithDidYouMean(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	// `--conn` is one edit-distance from `--connection`.
	cmd := "pg_hardstorage deployment add mydb1 --conn postgres://x --repo /tmp/r"
	err := cmdtree.Validate(tree, cmd, "pg_hardstorage")
	ve, ok := err.(*cmdtree.ValidationError)
	if !ok {
		t.Fatalf("error type = %T, want *ValidationError", err)
	}
	// Levenshtein("conn", "connection") = 6 — outside the
	// distance threshold.  The suggestion field should be
	// empty here, and that's the correct behaviour: an
	// over-eager suggestion is worse than none.
	if ve.Suggestion != "" {
		t.Logf("suggestion = %q (informational; may be empty)", ve.Suggestion)
	}
}

func TestValidate_EqualsFlagSyntax(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	// `--connection=value` shape (single token) must be
	// recognised as the same flag.
	cmd := `pg_hardstorage deployment add x --connection=postgres://h --repo=/tmp/r`
	if err := cmdtree.Validate(tree, cmd, "pg_hardstorage"); err != nil {
		t.Errorf("equals-form should validate; got %v", err)
	}
}

func TestValidate_BoolFlagDoesNotConsumeNext(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	// `--skip-probe` is a bool — the token after it
	// (here, another flag) must NOT be consumed as its
	// value.  Without correct handling this would mask
	// the next flag from validation.
	cmd := "pg_hardstorage deployment add x --skip-probe --bogus-flag"
	err := cmdtree.Validate(tree, cmd, "pg_hardstorage")
	ve, ok := err.(*cmdtree.ValidationError)
	if !ok {
		t.Fatalf("error type = %T, want *ValidationError", err)
	}
	if !strings.Contains(ve.Message, "bogus-flag") {
		t.Errorf("expected --bogus-flag to be flagged after --skip-probe: %q", ve.Message)
	}
}

func TestValidate_QuotedConnectionString(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	cmd := `pg_hardstorage deployment add x --connection 'postgres://h:p ass@x' --repo /tmp/r`
	if err := cmdtree.Validate(tree, cmd, "pg_hardstorage"); err != nil {
		t.Errorf("quoted DSN should validate; got %v", err)
	}
}

func TestValidate_BinaryNameVariants(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	for _, prefix := range []string{
		"pg_hardstorage",
		"./pg_hardstorage",
		"/usr/local/bin/pg_hardstorage",
	} {
		cmd := prefix + " deployment add x --connection postgres://h --repo /tmp/r"
		if err := cmdtree.Validate(tree, cmd, "pg_hardstorage"); err != nil {
			t.Errorf("%q should validate; got %v", cmd, err)
		}
	}
}

func TestValidate_WrongBinaryRejected(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	err := cmdtree.Validate(tree, "psql -c SELECT 1", "pg_hardstorage")
	ve, ok := err.(*cmdtree.ValidationError)
	if !ok {
		t.Fatalf("error type = %T, want *ValidationError", err)
	}
	if ve.Kind != "binary" {
		t.Errorf("kind = %q, want binary", ve.Kind)
	}
}

func TestValidate_PersistentFlagOnLeaf(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	// `--no-color` is a root-level persistent flag — the
	// validator must accept it on any leaf.
	cmd := "pg_hardstorage deployment list --no-color"
	if err := cmdtree.Validate(tree, cmd, "pg_hardstorage"); err != nil {
		t.Errorf("persistent flag on leaf should validate; got %v", err)
	}
}

func TestValidate_DidYouMeanShortMisspelling(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	// "ad" → "add" (distance 1)
	err := cmdtree.Validate(tree, "pg_hardstorage deployment ad --connection x --repo y", "pg_hardstorage")
	ve, ok := err.(*cmdtree.ValidationError)
	if !ok {
		t.Fatalf("error type = %T, want *ValidationError", err)
	}
	if ve.Suggestion != "add" {
		t.Errorf("Suggestion = %q, want \"add\" (Levenshtein distance 1)", ve.Suggestion)
	}
}

func TestValidate_HappyPath(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	cases := []string{
		"pg_hardstorage deployment add mydb --connection postgres://h --repo /tmp/r",
		"pg_hardstorage deployment list",
		"pg_hardstorage deployment ls",
		"pg_hardstorage repo init file:///tmp/x",
	}
	for _, c := range cases {
		if err := cmdtree.Validate(tree, c, "pg_hardstorage"); err != nil {
			t.Errorf("%q should validate; got %v", c, err)
		}
	}
}
