// builtins.go — always-safe LLM tools registered against DefaultRegistry.
package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
)

func init() {
	// Default registrations have nil Tree — the validating
	// path is unreachable.  buildLiveToolRegistry replaces
	// these with tree-bearing instances at session start.
	DefaultRegistry.Register(SuggestCommand{})
	DefaultRegistry.Register(&previewCommand{})
	DefaultRegistry.Register(&readRunbook{})
}

// SuggestCommand is the always-safe "tell the user what
// you'd run" tool.  It records the suggestion as a
// tool-result and the orchestrator surfaces it to the
// human; no command runs.
//
// When Tree is non-nil, the tool first validates the
// proposed command against the live cobra tree and
// returns a structured tool error on unknown subcommand
// / unknown flag with a did-you-mean hint, so the model
// retries with the right shape instead of dumping a
// fictional command on the operator.  The motivating
// case: model emits `pg_hardstorage deployment create
// --name X` (training-data convention), validator
// rejects with `unknown_command` naming "create" under
// "deployment" and listing the real verbs (add / edit /
// list / remove / test).  Model retries with
// `deployment add X --connection ... --repo ...`.
//
// When Tree is nil (the default-registry init() form),
// validation is skipped — the tool falls back to the
// pre-Layer-3 echo-only behaviour so tests that don't
// thread a cobra root keep working.
type SuggestCommand struct {
	// Tree is the frozen cobra command tree the tool
	// validates against.  Filled by the CLI layer at
	// session construction time.  nil disables validation.
	Tree *cmdtree.Node
}

// Name returns the tool identifier "suggest_command".
func (SuggestCommand) Name() string { return "suggest_command" }

// Description returns the model-facing summary of the tool's purpose.
func (SuggestCommand) Description() string {
	return "Suggest a CLI invocation to the user without executing it.  " +
		"The command is validated against the live `pg_hardstorage` " +
		"command tree before being shown to the operator — invalid " +
		"verbs / unknown flags are rejected with a did-you-mean hint " +
		"so you can retry with the right shape."
}

// Schema returns the JSON-schema-shaped argument descriptor.
func (SuggestCommand) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string"},
			"why":     map[string]any{"type": "string"},
		},
		"required": []string{"command", "why"},
	}
}

// ReadOnly reports that suggest_command does not mutate state.
func (SuggestCommand) ReadOnly() bool { return true }

// Run validates the proposed command against the cobra tree (when wired)
// and returns it as a suggestion the orchestrator surfaces to the operator.
func (s SuggestCommand) Run(_ context.Context, args map[string]any) (Result, error) {
	cmd, _ := args["command"].(string)
	why, _ := args["why"].(string)
	if cmd == "" {
		return Result{}, errors.New("suggest_command: command is required")
	}
	// Validation gate — the load-bearing piece.  When the
	// tree is wired (production), every suggestion is
	// parsed against it and unknown verbs / flags become
	// a structured tool error the model can react to.
	// When the tree is nil (test paths, MCP fallback), we
	// skip validation and preserve the echo-only contract.
	if s.Tree != nil {
		if err := cmdtree.Validate(s.Tree, cmd, "pg_hardstorage"); err != nil {
			ve, _ := err.(*cmdtree.ValidationError)
			if ve == nil {
				return Result{
					Summary: "suggest_command rejected: " + err.Error(),
					Body:    map[string]any{"error": "validation_failed", "message": err.Error()},
				}, nil
			}
			body := map[string]any{
				"error":             ve.Kind,
				"message":           ve.Message,
				"rejected_command":  cmd,
				"path_before_error": ve.PathBeforeError,
				"hint":              "the command did not parse against the live cobra tree; call read_command_help on the resolved prefix to get the real verb / flag list, then retry suggest_command with the corrected shape",
			}
			if ve.Suggestion != "" {
				body["did_you_mean"] = ve.Suggestion
			}
			return Result{
				Summary: fmt.Sprintf("suggest_command rejected (%s): %s", ve.Kind, ve.Message),
				Body:    body,
			}, nil
		}
	}
	return Result{
		Summary: "suggested: " + cmd,
		Body: map[string]string{
			"command": cmd,
			"why":     why,
		},
	}, nil
}

// PreviewCommandWithLedger is the dry-run rendering hook + the
// replay-protection ledger writer for advise+execute.
//
// Two roles:
//
//  1. Returns a structured "would-run" body so the model can
//     reason about the command's effect without executing.
//  2. When Ledger is non-nil (advise+execute mode wires it),
//     records the verbatim command string so a subsequent
//     execute_command in the same turn can reference it.
//
// Ledger is set by the chat orchestrator at session bootstrap;
// the per-turn Reset() clears it before each user turn so a
// stale preview from a previous turn can't be redeemed.
//
// The default registration (DefaultRegistry, init()) uses Ledger=nil
// — preview_command works (returns the would-run body) but
// doesn't prime execute_command.  The advise+execute path
// replaces the registry entry with a Ledger-bearing instance.
type PreviewCommandWithLedger struct {
	// Ledger records each previewed command for the
	// replay-protection check in execute_command.  nil means
	// execute_command is disabled (the read-only posture).
	Ledger interface{ Add(string) }
}

// previewCommand is the no-ledger instance kept for
// backwards compatibility with the init() registration.
type previewCommand struct{}

// Name returns the tool identifier "preview_command".
func (previewCommand) Name() string { return "preview_command" }

// Description returns the model-facing summary of the tool's purpose.
func (previewCommand) Description() string {
	return "Dry-run preview of a CLI invocation. Returns what the command would do without executing it.  In advise+execute mode this also primes execute_command's replay ledger — execute_command refuses any string that wasn't first passed here in the same turn."
}

// Schema returns the JSON-schema-shaped argument descriptor.
func (previewCommand) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string"},
		},
		"required": []string{"command"},
	}
}

// ReadOnly reports that preview_command does not mutate state.
func (previewCommand) ReadOnly() bool { return true }

// Run returns the would-run body for the proposed command without executing.
func (previewCommand) Run(_ context.Context, args map[string]any) (Result, error) {
	cmd, _ := args["command"].(string)
	if cmd == "" {
		return Result{}, errors.New("preview_command: command is required")
	}
	return Result{
		Summary: "preview: " + cmd,
		Body: map[string]string{
			"command":   cmd,
			"would_run": cmd,
		},
	}, nil
}

// Name returns the tool identifier "preview_command".
func (PreviewCommandWithLedger) Name() string { return "preview_command" }

// Description returns the model-facing summary of the tool's purpose.
func (p PreviewCommandWithLedger) Description() string { return previewCommand{}.Description() }

// Schema returns the JSON-schema-shaped argument descriptor.
func (PreviewCommandWithLedger) Schema() map[string]any { return previewCommand{}.Schema() }

// ReadOnly reports that preview_command does not mutate state.
func (PreviewCommandWithLedger) ReadOnly() bool { return true }

// Run returns the would-run body and records the command in the replay
// ledger so a subsequent execute_command in the same turn can reference it.
func (p PreviewCommandWithLedger) Run(_ context.Context, args map[string]any) (Result, error) {
	cmd, _ := args["command"].(string)
	if cmd == "" {
		return Result{}, errors.New("preview_command: command is required")
	}
	if p.Ledger != nil {
		p.Ledger.Add(cmd)
	}
	return Result{
		Summary: "preview: " + cmd,
		Body: map[string]string{
			"command":   cmd,
			"would_run": cmd,
		},
	}, nil
}

// readRunbook reads a shipped runbook by ID (R1..R7) and returns its
// markdown body. The runbook directory is the same one the agent's
// runbook-generator subsystem reads from; we resolve via the same
// docs/runbooks/ path inside the binary's install tree.
type readRunbook struct{}

// Name returns the tool identifier "read_runbook".
func (readRunbook) Name() string { return "read_runbook" }

// Description returns the model-facing summary of the tool's purpose.
func (readRunbook) Description() string {
	return "Read one of the shipped disaster-recovery runbooks (R1..R7) by ID."
}

// Schema returns the JSON-schema-shaped argument descriptor.
func (readRunbook) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "description": "Runbook ID, e.g. R3"},
		},
		"required": []string{"id"},
	}
}

// ReadOnly reports that read_runbook does not mutate state.
func (readRunbook) ReadOnly() bool { return true }

// Run loads the runbook by ID and returns its markdown body.
func (readRunbook) Run(_ context.Context, args map[string]any) (Result, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return Result{}, errors.New("read_runbook: id is required")
	}
	id = strings.ToUpper(strings.TrimSpace(id))
	body, source, err := loadRunbook(id)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Summary: fmt.Sprintf("runbook %s loaded (%d bytes)", id, len(body)),
		Body: map[string]any{
			"id":     id,
			"source": source,
			"body":   string(body),
		},
	}, nil
}

// loadRunbook resolves the on-disk markdown by walking the precedence
// chain: $PG_HARDSTORAGE_RUNBOOK_DIR > /usr/share/pg_hardstorage/runbooks
// > docs/runbooks (relative to cwd, helpful in development).
//
// We walk each directory and look for files starting with `<id>-`.
func loadRunbook(id string) ([]byte, string, error) {
	candidates := []string{
		os.Getenv("PG_HARDSTORAGE_RUNBOOK_DIR"),
		"/usr/share/pg_hardstorage/runbooks",
		"docs/runbooks",
	}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if strings.HasPrefix(e.Name(), id+"-") && (strings.HasSuffix(e.Name(), ".md") || strings.HasSuffix(e.Name(), ".markdown")) {
				path := filepath.Join(dir, e.Name())
				body, err := os.ReadFile(path)
				if err != nil {
					return nil, "", fmt.Errorf("read_runbook: %w", err)
				}
				return body, path, nil
			}
		}
	}
	return nil, "", fmt.Errorf("read_runbook: %s not found in any runbook directory (tried PG_HARDSTORAGE_RUNBOOK_DIR, /usr/share/pg_hardstorage/runbooks, docs/runbooks)", id)
}
