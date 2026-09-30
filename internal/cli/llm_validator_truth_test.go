package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
)

// The LLM helper validates every command it suggests against the live
// cobra tree and, on a warning, re-prompts the model to fix it. That
// loop is only as good as the validator's idea of what is valid.
//
// A 64-question eval found the validator rejecting commands the binary
// accepts, then instructing the model to "correct" them. Asked how to
// migrate from pgBackRest, the model correctly wrote
//
//	pg_hardstorage deployment add db1 --pg-connection '...'
//
// and was told --pg-connection is an unknown flag (it is a spelling
// alias for --connection there). On retry it produced --conn and
// --pg-conn — flags that really do not exist, and that the eval then
// counted as hallucinations. The validator was manufacturing the very
// error it exists to prevent.
//
// These tests hold the validator to the binary's actual behaviour.

func realTree(t *testing.T) *cmdtree.Node {
	t.Helper()
	return cmdtree.Walk(NewRoot())
}

// Everything here is accepted by the binary — verified by running it.
func TestValidator_AcceptsWhatTheBinaryAccepts(t *testing.T) {
	tree := realTree(t)
	for _, c := range []string{
		// spelling alias (acceptPGConnectionSpelling)
		"pg_hardstorage deployment add db1 --pg-connection 'host=x' --repo file:///r",
		"pg_hardstorage deployment add db1 --connection 'host=x' --repo file:///r",
		// --repo / --pg-connection back-filled from the named deployment
		"pg_hardstorage backup db1",
		"pg_hardstorage verify db1",
		"pg_hardstorage rotate db1 --apply",
	} {
		if err := cmdtree.Validate(tree, c, "pg_hardstorage"); err != nil {
			t.Errorf("%s\n  the binary accepts this; the validator said: %v", c, err)
		}
	}
}

// ...and these the binary rejects, so the validator must too. Loosening
// for aliases and back-fill must not open the door to invention.
func TestValidator_StillRejectsInventions(t *testing.T) {
	tree := realTree(t)
	for _, c := range []string{
		"pg_hardstorage deployment add db1 --conn 'host=x'",
		"pg_hardstorage deployment add db1 --pg-conn 'host=x'",
		"pg_hardstorage doctor --verbose",
		"pg_hardstorage backup db1 --conn x",
		"pg_hardstorage init --apply",
		"pg_hardstorage backup", // nothing to back-fill from
	} {
		if err := cmdtree.Validate(tree, c, "pg_hardstorage"); err == nil {
			t.Errorf("%s\n  the binary rejects this; the validator accepted it", c)
		}
	}
}

// TestBackfilledFlagsMatchThePreRunHook is the drift guard: the flags
// the validator treats as supplied-by-config must be exactly the ones
// the runtime pre-run hook actually fills. If someone teaches the hook
// a new flag, or the validator a new exemption, this fails.
func TestBackfilledFlagsMatchThePreRunHook(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_ROOT", root)
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "deployments:\n  db1:\n    pg_connection: 'host=h dbname=postgres'\n    repo: file:///r\n"
	if err := os.WriteFile(filepath.Join(root, "etc", "pg_hardstorage.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := &cobra.Command{Use: "probe"}
	for name := range cmdtree.DeploymentBackfilledFlags {
		cmd.Flags().String(name, "", "")
	}
	cmd.Flags().String("unrelated", "", "")
	if err := resolveDeploymentDefaultsPreRun(cmd, []string{"db1"}); err != nil {
		t.Fatal(err)
	}
	var filled []string
	for name := range cmdtree.DeploymentBackfilledFlags {
		if !cmd.Flags().Lookup(name).Changed {
			t.Errorf("validator exempts --%s when a deployment is named, but the pre-run hook did not fill it", name)
		} else {
			filled = append(filled, name)
		}
	}
	if cmd.Flags().Lookup("unrelated").Changed {
		t.Error("the hook filled a flag outside DeploymentBackfilledFlags; add it there or stop filling it")
	}
	if len(filled) == 0 {
		t.Fatal("nothing filled — the config fixture did not load: " + strings.Join(filled, ","))
	}
}

// Found by ultrareview: the back-fill exemption keyed on "any positional
// present", but the runtime hook only fills --repo when the first
// positional names a configured deployment. For commands whose
// positional is something else, --repo is still required at runtime,
// so the validator must still demand it.
func TestValidator_BackfillOnlyForDeploymentPositionals(t *testing.T) {
	tree := realTree(t)
	for _, c := range []string{
		"pg_hardstorage approval approve req-123",
		"pg_hardstorage audit verify-anchor log-1",
	} {
		if err := cmdtree.Validate(tree, c, "pg_hardstorage"); err == nil {
			t.Errorf("%s\n  its positional is not a deployment, so nothing back-fills --repo; the validator must reject it", c)
		}
	}
}

// Zero-argument mode: with no positional, the hook fills --repo from the
// one repo every deployment shares — and never --pg-connection. The
// validator's ZeroArgBackfilledFlags must say exactly that.
func TestZeroArgBackfillMatchesThePreRunHook(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_ROOT", root)
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "deployments:\n" +
		"  db1:\n    pg_connection: 'host=h dbname=postgres'\n    repo: file:///shared\n" +
		"  db2:\n    pg_connection: 'host=k dbname=postgres'\n    repo: file:///shared\n"
	if err := os.WriteFile(filepath.Join(root, "etc", "pg_hardstorage.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{Use: "probe"}
	cmd.Flags().String("repo", "", "")
	cmd.Flags().String("pg-connection", "", "")
	if err := resolveDeploymentDefaultsPreRun(cmd, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"repo", "pg-connection"} {
		filled := cmd.Flags().Lookup(name).Changed
		if filled != cmdtree.ZeroArgBackfilledFlags[name] {
			t.Errorf("--%s: hook filled=%v but validator exempts=%v in zero-arg mode",
				name, filled, cmdtree.ZeroArgBackfilledFlags[name])
		}
	}
}
