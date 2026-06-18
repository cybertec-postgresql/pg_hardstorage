// execute.go — ExecuteCommand: advise+execute LLM tool, gated by the four-layer safety stack.
package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/safety"
)

// ExecuteCommand is the advise+execute tool.  It runs a
// CLI invocation against the same pg_hardstorage binary that
// hosts the LLM session, but only after every safety gate
// passes:
//
//  1. ExecMode == ModeAdviseExecute (set by the CLI's
//     --mode flag).
//  2. SkillExecPolicy.Allowed(cmd)  — the active skill must
//     explicitly list this command's prefix in
//     allowed_executes.
//  3. ContainsMutationFlag refused  — never invoke a flag in
//     MutationFlagsRefused via the model; force the operator
//     to drop to a shell.
//  4. PreviewState.Has(cmd)         — replay protection;
//     execute_command only accepts a string the model JUST
//     asked preview_command to render, in the SAME turn.
//
// Tool result body:
//
//	{ "command": "...",
//	  "exit_code": 0,
//	  "stdout": "...",
//	  "stderr": "...",
//	  "matched_prefix": "pg_hardstorage doctor" }
//
// On a refusal the body's `refused: true` + `reason: "..."` lets
// the model report back accurately to the operator.
//
// Why ReadOnly() returns true even though this tool can mutate:
// the chat orchestrator's "non-read-only filter" predates
// advise+execute and was the belt-and-braces against ANY
// mutation tool reaching the model.  In advise+execute mode the
// gate moves: the orchestrator allows execute_command (because
// ReadOnly==true keeps it through the filter) but the tool's
// own Run() refuses unless ALL safety gates pass.  The
// orchestrator's filter remains useful as a defence against
// future tools that haven't yet been written to the safety
// contract.
type ExecuteCommand struct {
	// Mode is the ExecMode set by the CLI.  Default
	// ModeReadOnly causes Run to refuse every invocation
	// (the posture).
	Mode safety.ExecMode

	// Policy is the active skill's allowed_executes.  Empty
	// AllowedExecutes refuses every command.
	Policy safety.SkillExecPolicy

	// Preview is the per-turn preview ledger.  preview_command
	// writes; execute_command reads.  Required.
	Preview *safety.PreviewState

	// Anomaly is the 5th-gate detector.  When non-nil,
	// every command that passes the four hard gates is also
	// run through Anomaly.Score; Severe verdicts refuse, Warn
	// passes but emits an audit-event note.  When nil, the
	// anomaly check is skipped entirely (the posture).
	Anomaly *safety.AnomalyDetector

	// Runner is the CLI runner the actual exec routes through.
	// Required.  Same shape as the read-only tools' runner.
	Runner *CLIRunner

	// AuditCallback, when non-nil, fires on every gate
	// outcome (allow + refuse).  The chat orchestrator wires
	// this to its AuditEmitter so the trail records every
	// attempt.
	AuditCallback func(decision safety.GateDecision, command string)

	// AnomalyCallback, when non-nil, fires on every anomaly
	// verdict (Normal + Warn + Severe) so the chat
	// orchestrator can surface Warn-level notes in the audit
	// trail without refusing the command.
	AnomalyCallback func(decision safety.AnomalyDecision, command string)
}

// Name returns the tool's identifier.
func (ExecuteCommand) Name() string { return "execute_command" }

// Description tells the model what the tool does.
func (ExecuteCommand) Description() string {
	return "Execute a pg_hardstorage CLI command.  Refuses unless the operator started the session with --mode advise+execute, the active skill's allowed_executes covers the prefix, the command contains no mutation flags, and preview_command rendered the same string earlier in this turn."
}

// Schema declares the tool's argument shape for the provider.
func (ExecuteCommand) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The full CLI invocation (e.g. \"pg_hardstorage doctor -d db1\").  Must EXACTLY match a string passed to preview_command in this same turn.",
			},
		},
		"required": []string{"command"},
	}
}

// ReadOnly: see the package comment above for why this is true
// despite the tool's name.
func (ExecuteCommand) ReadOnly() bool { return true }

// Run drives the safety gate, invokes the runner on success,
// fires the audit callback on every outcome.
func (e *ExecuteCommand) Run(ctx context.Context, args map[string]any) (Result, error) {
	cmd, _ := args["command"].(string)
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return Result{}, errors.New("execute_command: command is required")
	}
	decision := safety.Gate(e.Mode, e.Policy, e.Preview, cmd)
	if e.AuditCallback != nil {
		e.AuditCallback(decision, cmd)
	}
	if !decision.Allowed {
		return Result{
			Summary: "execute_command refused",
			Body: map[string]any{
				"refused": true,
				"reason":  safety.Reason(decision),
				"command": cmd,
			},
		}, nil
	}

	// Fifth gate: anomaly-refusal.  Runs after the
	// four hard gates because cheap things first.  A Severe
	// verdict refuses outright; Warn lets the command
	// through but the audit chain records the note.  Normal
	// passes silently.
	if e.Anomaly != nil {
		anom := e.Anomaly.Score(cmd)
		if e.AnomalyCallback != nil {
			e.AnomalyCallback(anom, cmd)
		}
		if anom.Score == safety.ScoreSevere {
			return Result{
				Summary: "execute_command refused (anomaly)",
				Body: map[string]any{
					"refused":       true,
					"reason":        "anomaly: " + anom.Reason,
					"anomaly_verb":  anom.Verb,
					"anomaly_token": anom.Token,
					"command":       cmd,
				},
			}, nil
		}
	}

	// Invoke the runner.  We split the command into argv
	// tokens by whitespace (no shell parsing — operators
	// rendering complex quoting via the model are doing
	// something off-policy already).
	tokens := strings.Fields(cmd)
	if len(tokens) == 0 {
		return Result{}, errors.New("execute_command: empty after tokenisation")
	}
	// Strip the leading binary name — the runner already
	// invokes pg_hardstorage at e.Runner.Path.
	argv := tokens[1:]
	if len(tokens) > 0 && !strings.HasSuffix(tokens[0], "pg_hardstorage") {
		// Operator-allowlisted prefix that doesn't start with
		// the binary name?  We refuse — execute_command is
		// scoped to pg_hardstorage subcommands.
		return Result{
			Summary: "execute_command refused",
			Body: map[string]any{
				"refused": true,
				"reason":  fmt.Sprintf("command must start with the pg_hardstorage binary; got %q", tokens[0]),
				"command": cmd,
			},
		}, nil
	}

	// Use the same JSON output mode the read-only tools use so
	// the model can reason about the result programmatically.
	body, err := e.Runner.RunJSON(ctx, argv...)
	if err != nil {
		return Result{
			Summary: "execute_command failed",
			Body: map[string]any{
				"command": cmd,
				"error":   err.Error(),
				"stdout":  string(body),
			},
		}, nil
	}
	return Result{
		Summary: fmt.Sprintf("executed: %s", cmd),
		Body: map[string]any{
			"command":        cmd,
			"matched_prefix": decision.MatchedPrefix,
			"output":         string(body),
		},
	}, nil
}
