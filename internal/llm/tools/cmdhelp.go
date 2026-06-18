// cmdhelp.go — read_command_help tool: synopsis + flag list from the cobra tree.
package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
)

// ReadCommandHelp is the LLM-side cousin of `pg_hardstorage
// <command> --help`.  Given a command path the model wants
// to suggest, returns the synopsis + flag list rendered
// from the live cobra tree.
//
// The motivating use case: the system prompt's command
// catalog (see Layer 1) shows the model the verb tree but
// omits flags — there are 200+ of them across the binary.
// When the model needs to suggest a specific command, it
// calls this tool to get the flag list and pick the right
// flag names instead of guessing (`--connection` vs
// `--conn`, `--repo` vs `--repository`, `--name` that
// doesn't exist because the name is positional).
//
// Wired by buildLiveToolRegistry with a non-nil Tree.
// When Tree is nil (e.g. a test that builds a registry
// without the cobra root), the tool returns a structured
// "tool unavailable" Result so a model that requests it
// gets a clear answer instead of a generic-looking
// failure.
type ReadCommandHelp struct {
	// Tree is the frozen cobra command tree the tool
	// queries.  Filled by the CLI layer at session
	// construction time via cmdtree.Walk(cmd.Root()).
	Tree *cmdtree.Node
}

// Name returns the tool identifier "read_command_help".
func (ReadCommandHelp) Name() string { return "read_command_help" }

// Description returns the model-facing summary of the tool's purpose.
func (ReadCommandHelp) Description() string {
	return "Look up the synopsis + flag list for a real `pg_hardstorage` command. " +
		"Pass the command path as a single string ('deployment add', 'wal stream', " +
		"'repo init').  Use this BEFORE suggesting an unfamiliar command — the " +
		"system-prompt catalog shows verbs but not flags, and improvising flag " +
		"names is the most common source of broken suggestions."
}

// Schema returns the JSON-schema-shaped argument descriptor.
func (ReadCommandHelp) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "Command path without the binary name (e.g. 'deployment add', 'repo init').",
			},
		},
		"required": []string{"command"},
	}
}

// ReadOnly reports that read_command_help does not mutate state.
func (ReadCommandHelp) ReadOnly() bool { return true }

// Run resolves the command path against the cobra tree and returns the
// rendered help text, or a structured "not found" body with a hint when
// the path does not resolve.
func (h ReadCommandHelp) Run(_ context.Context, args map[string]any) (Result, error) {
	cmd, _ := args["command"].(string)
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return Result{}, errors.New("read_command_help: command is required")
	}
	// Tolerate the model passing the binary name as a
	// prefix — a forgivable mistake on the model's part
	// since real `--help` invocations include it.
	cmd = strings.TrimPrefix(cmd, "pg_hardstorage ")
	cmd = strings.TrimSpace(cmd)

	if h.Tree == nil {
		return Result{
			Summary: "read_command_help: tool unavailable in this session (no command tree wired)",
			Body: map[string]any{
				"error": "tool_unavailable",
				"hint":  "this build of pg_hardstorage was started without a command-tree introspector — fall back to the catalog block in the system prompt",
			},
		}, nil
	}

	path := strings.Fields(cmd)
	help := cmdtree.Help(h.Tree, path)
	if help == "" {
		// Resolve as far as possible to give the model a
		// useful "did you mean" surface.  We re-walk the
		// path one segment at a time and report where the
		// resolution broke.
		resolved, available := resolvePartial(h.Tree, path)
		return Result{
			Summary: fmt.Sprintf("read_command_help: %q not found", cmd),
			Body: map[string]any{
				"error":               "command_not_found",
				"requested":           cmd,
				"resolved_prefix":     strings.Join(resolved, " "),
				"available_at_prefix": available,
				"hint":                "the path you passed does not resolve in the cobra command tree; pick one of available_at_prefix or call read_command_help with a higher-level path",
			},
		}, nil
	}
	return Result{
		Summary: fmt.Sprintf("help: %s", strings.Join(path, " ")),
		Body: map[string]any{
			"command": strings.Join(path, " "),
			"help":    help,
		},
	}, nil
}

// resolvePartial walks the tree along path until a segment
// fails to resolve, returning what DID resolve and what
// children are available at that depth so the model has a
// useful next step to try.
func resolvePartial(root *cmdtree.Node, path []string) (resolved []string, available []string) {
	cur := root
	for _, seg := range path {
		next := cur.Find([]string{seg})
		if next == nil {
			break
		}
		resolved = append(resolved, seg)
		cur = next
	}
	for _, c := range cur.VisibleChildren() {
		available = append(available, c.Name)
	}
	return resolved, available
}
