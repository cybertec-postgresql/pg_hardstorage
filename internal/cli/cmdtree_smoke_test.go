package cli_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
)

// TestCmdtree_RealRootSmoke walks the production cobra
// root and asserts the introspector handles its size +
// shape without panicking, AND that the operator's
// reported bug — `pg_hardstorage deployment create
// --name mydb1` — is rejected with a useful error.  This
// test lives in internal/cli (not internal/cli/cmdtree)
// because it needs cli.NewRoot() and importing it from
// inside cmdtree would create a layering cycle.
func TestCmdtree_RealRootSmoke(t *testing.T) {
	root := cli.NewRoot()
	tree := cmdtree.Walk(root)
	if tree == nil {
		t.Fatal("Walk returned nil for live root")
	}
	if len(tree.Children) < 20 {
		t.Errorf("expected dozens of top-level commands; got %d", len(tree.Children))
	}

	// Sanity — the real `deployment add` is reachable.
	add := tree.Find([]string{"deployment", "add"})
	if add == nil {
		t.Fatal("deployment add must exist on the live tree")
	}
	if add.FlagByName("connection") == nil {
		t.Errorf("deployment add must have --connection flag")
	}

	// The actual bug, end-to-end.
	bug := "pg_hardstorage deployment create --name mydb1 --connection 'postgres://hs:pass@localhost:5432/postgres' --repo /tmp/repo"
	err := cmdtree.Validate(tree, bug, "pg_hardstorage")
	if err == nil {
		t.Fatal("validator missed the unknown-subcommand bug; expected unknown_command")
	}
	ve, ok := err.(*cmdtree.ValidationError)
	if !ok {
		t.Fatalf("error type = %T, want *cmdtree.ValidationError", err)
	}
	if ve.Kind != "unknown_command" || !strings.Contains(ve.Message, "create") {
		t.Errorf("validator returned %q / %q; want unknown_command naming 'create'", ve.Kind, ve.Message)
	}

	// Catalog rendering should be small enough to fit in
	// a system prompt without dominating it.  ~10 KiB at
	// depth 2 is well within budget; depth 1 (top-level
	// only) is the safe fallback if budget is tight.
	cat := cmdtree.Catalog(tree, 2)
	if len(cat) == 0 {
		t.Fatal("catalog rendered empty")
	}
	if len(cat) > 32*1024 {
		t.Errorf("depth-2 catalog is %d bytes — too large for a system prompt", len(cat))
	}
}
