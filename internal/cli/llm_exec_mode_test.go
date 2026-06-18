package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/safety"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/skills"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func TestResolveExecMode_DefaultReadOnly(t *testing.T) {
	mode, state, err := resolveExecMode("", &skills.Skill{Name: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if mode != safety.ModeReadOnly {
		t.Errorf("mode = %q, want read-only", mode)
	}
	if state != nil {
		t.Errorf("state should be nil in read-only; got %+v", state)
	}
}

func TestResolveExecMode_ReadOnlyAlias(t *testing.T) {
	for _, in := range []string{"read-only", "readonly", "READ-ONLY"} {
		mode, _, err := resolveExecMode(in, &skills.Skill{Name: "ask"})
		if err != nil {
			t.Errorf("input %q: %v", in, err)
		}
		if mode != safety.ModeReadOnly {
			t.Errorf("input %q → mode %q, want read-only", in, mode)
		}
	}
}

func TestResolveExecMode_UnrecognisedRefused(t *testing.T) {
	_, _, err := resolveExecMode("unknown-mode", &skills.Skill{Name: "ask"})
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
	var oerr *output.Error
	if !errors.As(err, &oerr) || oerr.Code != "usage.bad_mode" {
		t.Errorf("expected usage.bad_mode; got %v", err)
	}
}

func TestResolveExecMode_AdviseRefusedWithoutExecuteCommand(t *testing.T) {
	skill := &skills.Skill{
		Name: "ask",
		Context: skills.ContextSpec{
			AvailableTools: []string{"read_doctor"},
			// execute_command NOT in AvailableTools
			AllowedExecutes: []string{"pg_hardstorage doctor"},
		},
	}
	_, _, err := resolveExecMode("advise+execute", skill)
	if err == nil {
		t.Fatal("expected refusal when skill lacks execute_command")
	}
	if !strings.Contains(err.Error(), "execute_command") {
		t.Errorf("error should explain the missing tool; got %v", err)
	}
}

func TestResolveExecMode_AdviseRefusedWithEmptyAllowedExecutes(t *testing.T) {
	skill := &skills.Skill{
		Name: "loose",
		Context: skills.ContextSpec{
			AvailableTools: []string{"execute_command"},
			// AllowedExecutes empty
		},
	}
	_, _, err := resolveExecMode("advise+execute", skill)
	if err == nil {
		t.Fatal("expected refusal when AllowedExecutes is empty")
	}
	if !strings.Contains(err.Error(), "allowed_executes") {
		t.Errorf("error should explain the empty allowlist; got %v", err)
	}
}

func TestResolveExecMode_AdviseHappyPath(t *testing.T) {
	skill := &skills.Skill{
		Name: "ops",
		Context: skills.ContextSpec{
			AvailableTools: []string{"read_doctor", "execute_command"},
			AllowedExecutes: []string{
				"pg_hardstorage doctor",
				"pg_hardstorage list",
			},
		},
	}
	mode, state, err := resolveExecMode("advise+execute", skill)
	if err != nil {
		t.Fatal(err)
	}
	if mode != safety.ModeAdviseExecute {
		t.Errorf("mode = %q, want advise+execute", mode)
	}
	if state == nil {
		t.Fatal("state should be non-nil under advise+execute")
	}
	if state.mode != safety.ModeAdviseExecute {
		t.Errorf("state.mode = %q", state.mode)
	}
	if len(state.policy.AllowedExecutes) != 2 {
		t.Errorf("policy.AllowedExecutes count = %d, want 2", len(state.policy.AllowedExecutes))
	}
	if state.preview == nil {
		t.Error("state.preview should be initialised")
	}
}

func TestResolveExecMode_AcceptsAliases(t *testing.T) {
	skill := &skills.Skill{
		Name: "ops",
		Context: skills.ContextSpec{
			AvailableTools:  []string{"execute_command"},
			AllowedExecutes: []string{"pg_hardstorage doctor"},
		},
	}
	for _, in := range []string{"advise+execute", "advise-execute", "advise_execute", "ADVISE+EXECUTE"} {
		mode, state, err := resolveExecMode(in, skill)
		if err != nil {
			t.Errorf("input %q: %v", in, err)
			continue
		}
		if mode != safety.ModeAdviseExecute || state == nil {
			t.Errorf("input %q → mode %q state %v", in, mode, state)
		}
	}
}
