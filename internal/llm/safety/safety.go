// Package safety implements the advise+execute mode's defense-
// in-depth gates.  When a skill is loaded under
// advise+execute (off by default; opt-in via --mode flag), the
// model can request `execute_command` to run a CLI invocation.
// Before the run can fire, the safety stack enforces:
//
//  1. Replay protection — execute_command will only accept a
//     command string that the model JUST asked preview_command
//     to render, in the SAME turn.  The model cannot construct
//     arbitrary strings that bypass preview.
//  2. Mutation-flag refusal — the binary's own approval gates
//     / typed-confirmation flags / RBAC are still authoritative,
//     but execute_command refuses any command containing flags
//     we don't expect a read-only-skill model to construct
//     (--apply, --yes, --force, etc.) UNLESS the operator
//     pre-authorised them via a separate console gesture.
//  3. Anomaly refusal — the skill's declared `available_tools`
//     list bounds what execute_command may run.  A command
//     that doesn't match the skill's `allowed_executes` (a
//     prefix list, e.g. ["pg_hardstorage status",
//     "pg_hardstorage list", "pg_hardstorage doctor"]) is
//     refused at the skill boundary.
//  4. Audit on every gate — every execute, refusal, and
//     bypass attempt fires a chat.AuditEvent so the chain
//     records WHO got past the gates and WHAT they ran.
//
// The package is the policy engine; the actual exec.Command
// invocation lives in the cli package (where the
// pg_hardstorage binary path is resolvable).  The two are
// kept separate so the safety logic is testable without
// shelling out.
package safety

import (
	"errors"
	"fmt"
	"strings"
)

// ExecMode is the runtime gate for execute_command.
type ExecMode string

const (
	// ModeReadOnly is the default and the posture: even
	// when a skill declares execute_command in its
	// available_tools, the gate refuses every invocation.
	// This exists so a future skill that asks for execute_command
	// can be loaded without lighting up the actual execution
	// path until is reached.
	ModeReadOnly ExecMode = "read-only"

	// ModeAdviseExecute is the opt-in.  execute_command
	// is permitted when every gate passes.  Operators flip
	// this with --mode advise+execute on the chat command.
	ModeAdviseExecute ExecMode = "advise+execute"
)

// PreviewState is the per-turn replay-protection ledger.
// preview_command writes; execute_command reads.  The state is
// reset at every user turn so a stale preview from a previous
// turn can't be redeemed.
type PreviewState struct {
	// Commands collected this turn.  Each entry is the verbatim
	// string the preview_command tool was asked to render.
	Commands []string
}

// Add records a fresh preview.  Called by preview_command's
// Run() in advise+execute mode.
func (p *PreviewState) Add(cmd string) {
	if p == nil {
		return
	}
	p.Commands = append(p.Commands, cmd)
}

// Has reports whether cmd was previewed this turn.  Match is
// EXACT — no normalisation, no whitespace tolerance.  The model
// must hand execute_command the literal bytes preview_command
// just received.
func (p *PreviewState) Has(cmd string) bool {
	if p == nil {
		return false
	}
	for _, c := range p.Commands {
		if c == cmd {
			return true
		}
	}
	return false
}

// Reset clears the ledger.  Called at the start of each user
// turn so cross-turn replay is impossible.
func (p *PreviewState) Reset() {
	if p == nil {
		return
	}
	p.Commands = nil
}

// SkillExecPolicy is the per-skill execution allowlist.  A
// skill that declares execute_command in available_tools MUST
// also declare allowed_executes — a list of command-prefix
// strings.  Any execute_command invocation must START WITH one
// of these prefixes (after trimming whitespace).
//
// Example:
//
//	allowed_executes:
//	  - "pg_hardstorage doctor"
//	  - "pg_hardstorage list"
//	  - "pg_hardstorage status"
//
// This bounds what a chatty model can ask to run even after
// preview_command + execute_command both fire — the skill
// author decides the blast radius.
type SkillExecPolicy struct {
	AllowedExecutes []string
}

// Allowed reports whether cmd is permitted under the skill
// policy.  Empty AllowedExecutes refuses every command (a
// skill that hasn't explicitly listed permitted prefixes
// shouldn't get to execute anything).
func (p SkillExecPolicy) Allowed(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" || len(p.AllowedExecutes) == 0 {
		return false
	}
	for _, prefix := range p.AllowedExecutes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		if cmd == prefix || strings.HasPrefix(cmd, prefix+" ") {
			return true
		}
	}
	return false
}

// MutationFlagsRefused is the centralised list of flags the
// safety stack refuses at the execute_command boundary, even
// when every other gate passes.  Authoritative defence
// remains the binary's own gates (approval workflows, typed-
// confirmation, RBAC) — this list catches the easy mistakes.
//
// Operators with a legitimate need to run a destructive command
// from within an LLM session should drop to the shell, run it
// manually, and return.  The plan's design rationale: "The
// LLM cannot type for them."
var MutationFlagsRefused = []string{
	"--apply",
	"--yes",
	"--force",
	"--reset-chain-staging",
	"--confirm-keyring",
	"--require-approval",
	"--skip-verify",
	"--skip-gap-check",
}

// ErrMutationFlag is returned when the proposed command
// contains a flag from MutationFlagsRefused.  Wrapped by the
// gate so callers can errors.Is against it.
var ErrMutationFlag = errors.New("safety: command contains a mutation flag the LLM gate refuses")

// ContainsMutationFlag reports whether cmd contains any
// MutationFlagsRefused flag as a whitespace-delimited token.
// Returns the first matching flag for the error message.
func ContainsMutationFlag(cmd string) (string, bool) {
	tokens := strings.Fields(cmd)
	for _, t := range tokens {
		// Strip "=value" suffix so --yes=true matches --yes.
		bare := t
		if idx := strings.IndexByte(bare, '='); idx > 0 {
			bare = bare[:idx]
		}
		for _, refused := range MutationFlagsRefused {
			if bare == refused {
				return refused, true
			}
		}
	}
	return "", false
}

// GateDecision captures the outcome of the safety gate stack
// for one execute_command attempt.  Returned to the caller for
// audit emission + the user-facing message.
type GateDecision struct {
	// Allowed is true only when ALL gates passed.
	Allowed bool

	// Reason is the gate that fired (when Allowed is false).
	// Operators see this in the audit trail.
	Reason string

	// MatchedPrefix is the SkillExecPolicy prefix that allowed
	// the command (when Allowed is true).  Empty otherwise.
	MatchedPrefix string

	// MutationFlag is the flag that fired the mutation refusal
	// (when Reason == "mutation_flag").  Empty otherwise.
	MutationFlag string
}

// Gate evaluates every safety gate for cmd and returns the
// decision.  Order matters — earlier gates fire first because
// they're cheapest:
//
//  1. ExecMode == ModeReadOnly        → "read_only_mode"
//  2. SkillExecPolicy.Allowed(cmd)    → "skill_disallowed"
//  3. ContainsMutationFlag(cmd)       → "mutation_flag"
//  4. PreviewState.Has(cmd)           → "no_preview"
//
// Only after all four pass does Allowed become true.
func Gate(mode ExecMode, policy SkillExecPolicy, preview *PreviewState, cmd string) GateDecision {
	if mode != ModeAdviseExecute {
		return GateDecision{Reason: "read_only_mode"}
	}
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return GateDecision{Reason: "empty_command"}
	}
	if !policy.Allowed(cmd) {
		return GateDecision{Reason: "skill_disallowed"}
	}
	if flag, has := ContainsMutationFlag(cmd); has {
		return GateDecision{
			Reason:       "mutation_flag",
			MutationFlag: flag,
		}
	}
	if !preview.Has(cmd) {
		return GateDecision{Reason: "no_preview"}
	}
	return GateDecision{
		Allowed:       true,
		MatchedPrefix: matchedPrefix(policy, cmd),
	}
}

func matchedPrefix(policy SkillExecPolicy, cmd string) string {
	cmd = strings.TrimSpace(cmd)
	for _, prefix := range policy.AllowedExecutes {
		prefix = strings.TrimSpace(prefix)
		if cmd == prefix || strings.HasPrefix(cmd, prefix+" ") {
			return prefix
		}
	}
	return ""
}

// Reason maps a GateDecision.Reason to a human-readable
// explanation.  Used by the chat orchestrator's tool-result
// echo + the audit body.
func Reason(d GateDecision) string {
	switch d.Reason {
	case "read_only_mode":
		return "execute_command is disabled — start the session with --mode advise+execute to enable"
	case "skill_disallowed":
		return "skill policy refuses this command (not in allowed_executes)"
	case "mutation_flag":
		return fmt.Sprintf("command contains the mutation flag %q which the LLM gate refuses; run it manually from a shell", d.MutationFlag)
	case "no_preview":
		return "execute_command refused: no matching preview_command in this turn (replay protection)"
	case "empty_command":
		return "command is empty"
	default:
		return "(unknown gate)"
	}
}
