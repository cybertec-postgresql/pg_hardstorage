package tools

import (
	"context"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/safety"
)

// execTree is a miniature of the real root: the side-effect persistent
// flags plus a runnable `doctor [<deployment>]`.
func execTree() *cmdtree.Node {
	root := &cobra.Command{Use: "pg_hardstorage"}
	root.PersistentFlags().StringP("config", "c", "", "")
	root.PersistentFlags().StringP("output", "o", "", "")
	root.PersistentFlags().String("cpu-profile", "", "")
	root.PersistentFlags().BoolP("quiet", "q", false, "")
	root.AddCommand(&cobra.Command{Use: "doctor [<deployment>]", Run: func(*cobra.Command, []string) {}})
	return cmdtree.Walk(root)
}

func newExec(t *testing.T, tree *cmdtree.Node, cmd string) (*ExecuteCommand, *stubRunner) {
	t.Helper()
	captured := &stubRunner{stdout: map[string][]byte{"doctor": []byte(`{}`)}, exitCode: map[string]int{}}
	prev := &safety.PreviewState{}
	prev.Add(cmd)
	return &ExecuteCommand{
		Mode:    safety.ModeAdviseExecute,
		Policy:  safety.SkillExecPolicy{AllowedExecutes: []string{"pg_hardstorage doctor"}},
		Preview: prev,
		Runner:  &CLIRunner{Path: "/x", Runner: captured.run},
		Tree:    tree,
	}, captured
}

// execute_command must parse the proposed command against the real
// cobra tree and refuse unknown flags / subcommands — the prefix
// allowlist alone lets anything ride after "pg_hardstorage doctor".
func TestExecuteCommand_RefusesUnknownFlag(t *testing.T) {
	for _, cmd := range []string{
		"pg_hardstorage doctor --bogus-flag",
		"pg_hardstorage doctor --cpu-profile=/home/op/.bashrc",
		"pg_hardstorage doctor db1 extra",
	} {
		e, captured := newExec(t, execTree(), cmd)
		res, err := e.Run(context.Background(), map[string]any{"command": cmd})
		if err != nil {
			t.Fatalf("%q: %v", cmd, err)
		}
		body, _ := res.Body.(map[string]any)
		if body["refused"] != true {
			t.Errorf("%q: not refused: %+v", cmd, res)
		}
		if len(captured.calls) != 0 {
			t.Errorf("%q: runner invoked with %v", cmd, captured.calls)
		}
	}
	// A valid command still runs.
	cmd := "pg_hardstorage doctor db1 -q"
	e, captured := newExec(t, execTree(), cmd)
	if _, err := e.Run(context.Background(), map[string]any{"command": cmd}); err != nil {
		t.Fatal(err)
	}
	if len(captured.calls) != 1 {
		t.Errorf("valid command should run; calls = %v", captured.calls)
	}
}

// Without the command tree the parse gate cannot run, so the tool
// refuses rather than degrading to prefix-only checking.
func TestExecuteCommand_RefusesWithoutTree(t *testing.T) {
	cmd := "pg_hardstorage doctor"
	e, captured := newExec(t, nil, cmd)
	res, err := e.Run(context.Background(), map[string]any{"command": cmd})
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := res.Body.(map[string]any); body["refused"] != true {
		t.Errorf("nil tree: not refused: %+v", res)
	}
	if len(captured.calls) != 0 {
		t.Errorf("runner invoked: %v", captured.calls)
	}
}
