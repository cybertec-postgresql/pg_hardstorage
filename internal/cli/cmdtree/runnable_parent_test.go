package cmdtree_test

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
)

// runnableParentRoot mirrors the real CLI shape that broke the LLM
// command-validator: `backup <deployment>` is BOTH runnable (takes a
// positional) AND the parent of `backup delete`, and carries a required
// flag. Before the Runnable fix, `backup db1` was rejected as
// "unknown subcommand db1" — masking the genuinely useful
// missing-required-flag check.
func runnableParentRoot() *cobra.Command {
	root := &cobra.Command{Use: "pg_hardstorage"}
	backup := &cobra.Command{
		Use: "backup <deployment>",
		Run: func(_ *cobra.Command, _ []string) {},
	}
	backup.Flags().String("pg-connection", "", "libpq connection (required)")
	_ = backup.MarkFlagRequired("pg-connection")
	backup.Flags().String("repo", "", "repository URL")
	backup.AddCommand(&cobra.Command{
		Use: "delete <deployment> <id>",
		Run: func(_ *cobra.Command, _ []string) {},
	})
	root.AddCommand(backup)
	return root
}

// TestValidate_RunnableParentPositionalIsNotUnknownSubcommand: `backup db1`
// must NOT be flagged as an unknown subcommand — db1 is a positional. The
// validator must instead reach the required-flag check and report the
// missing --pg-connection (the actually-useful signal that was masked).
func TestValidate_RunnableParentPositionalIsNotUnknownSubcommand(t *testing.T) {
	tree := cmdtree.Walk(runnableParentRoot())
	err := cmdtree.Validate(tree, "pg_hardstorage backup db1", "pg_hardstorage")
	if err == nil {
		t.Fatal("expected missing_required for --pg-connection, got nil")
	}
	ve, ok := err.(*cmdtree.ValidationError)
	if !ok {
		t.Fatalf("wrong error type: %T (%v)", err, err)
	}
	if ve.Kind != "missing_required" {
		t.Errorf("Kind = %q, want missing_required (got: %v)", ve.Kind, ve)
	}
	if !strings.Contains(ve.Message, "pg-connection") {
		t.Errorf("missing_required should name --pg-connection, got: %v", ve)
	}
}

// TestValidate_RunnableParentWithRequiredFlagPasses: the same command with
// the required flag supplied is valid.
func TestValidate_RunnableParentWithRequiredFlagPasses(t *testing.T) {
	tree := cmdtree.Walk(runnableParentRoot())
	cmd := "pg_hardstorage backup db1 --pg-connection postgres://x --repo file:///r"
	if err := cmdtree.Validate(tree, cmd, "pg_hardstorage"); err != nil {
		t.Errorf("valid runnable-parent command should pass, got: %v", err)
	}
}

// TestValidate_RealSubcommandStillResolves: a genuine subcommand under a
// runnable parent still resolves (we didn't turn off subcommand walking).
func TestValidate_RealSubcommandStillResolves(t *testing.T) {
	tree := cmdtree.Walk(runnableParentRoot())
	if err := cmdtree.Validate(tree, "pg_hardstorage backup delete db1 latest", "pg_hardstorage"); err != nil {
		t.Errorf("`backup delete db1 latest` should resolve, got: %v", err)
	}
}

// TestValidate_NonRunnableParentStillCatchesTypos: a pure parent (no Run)
// must still reject an unknown subcommand — we only relaxed the check for
// runnable commands.
func TestValidate_NonRunnableParentStillCatchesTypos(t *testing.T) {
	root := &cobra.Command{Use: "pg_hardstorage"}
	dep := &cobra.Command{Use: "deployment"} // no Run → pure parent
	dep.AddCommand(&cobra.Command{Use: "add <name>", Run: func(_ *cobra.Command, _ []string) {}})
	root.AddCommand(dep)
	tree := cmdtree.Walk(root)
	err := cmdtree.Validate(tree, "pg_hardstorage deployment creat", "pg_hardstorage")
	ve, ok := err.(*cmdtree.ValidationError)
	if !ok || ve.Kind != "unknown_command" {
		t.Fatalf("expected unknown_command for typo under a pure parent, got: %v", err)
	}
}

// TestValidate_UsageDeclaredRequiredFlag: many pg_hardstorage commands
// enforce --repo / --pg-connection manually in RunE rather than via
// MarkFlagRequired, so the only signal is "(required)" in the help text.
// cmdtree must treat that as required so the validator catches a dropped
// flag (F2 end-to-end).
func TestValidate_UsageDeclaredRequiredFlag(t *testing.T) {
	root := &cobra.Command{Use: "pg_hardstorage"}
	rot := &cobra.Command{Use: "rotate <deployment>", Run: func(_ *cobra.Command, _ []string) {}}
	// No MarkFlagRequired — required-ness is only in the usage string.
	rot.Flags().String("repo", "", "repository URL (file://, s3://, ...) — must already exist (required)")
	rot.Flags().Bool("apply", false, "actually soft-delete (default: dry-run)")
	root.AddCommand(rot)
	tree := cmdtree.Walk(root)

	err := cmdtree.Validate(tree, "pg_hardstorage rotate db1 --apply", "pg_hardstorage")
	ve, ok := err.(*cmdtree.ValidationError)
	if !ok || ve.Kind != "missing_required" || !strings.Contains(ve.Message, "repo") {
		t.Fatalf("usage-declared required flag not enforced; got: %v", err)
	}
	if err := cmdtree.Validate(tree, "pg_hardstorage rotate db1 --repo file:///r --apply", "pg_hardstorage"); err != nil {
		t.Errorf("with --repo supplied it should pass, got: %v", err)
	}
}

// TestWalk_RunnableIsCaptured: Walk records Runnable from cobra.
func TestWalk_RunnableIsCaptured(t *testing.T) {
	tree := cmdtree.Walk(runnableParentRoot())
	var backup *cmdtree.Node
	for _, c := range tree.Children {
		if c.Name == "backup" {
			backup = c
		}
	}
	if backup == nil {
		t.Fatal("backup node missing")
	}
	if !backup.Runnable {
		t.Error("backup should be Runnable (it has a Run func)")
	}
	for _, c := range backup.Children {
		if c.Name == "delete" && !c.Runnable {
			t.Error("backup delete should be Runnable")
		}
	}
}
