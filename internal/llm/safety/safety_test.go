package safety_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/safety"
)

func TestPreviewState_AddHasReset(t *testing.T) {
	var p safety.PreviewState
	if p.Has("anything") {
		t.Error("empty state should report no previews")
	}
	p.Add("pg_hardstorage status db1")
	p.Add("pg_hardstorage doctor")
	if !p.Has("pg_hardstorage status db1") {
		t.Error("Has should find first added")
	}
	if !p.Has("pg_hardstorage doctor") {
		t.Error("Has should find second added")
	}
	if p.Has("pg_hardstorage status db2") {
		t.Error("Has match must be exact (db1 != db2)")
	}
	p.Reset()
	if p.Has("pg_hardstorage status db1") {
		t.Error("Reset should clear")
	}
}

func TestSkillExecPolicy_Allowed(t *testing.T) {
	p := safety.SkillExecPolicy{
		AllowedExecutes: []string{
			"pg_hardstorage doctor",
			"pg_hardstorage list",
			"pg_hardstorage status",
		},
	}
	cases := []struct {
		cmd  string
		want bool
	}{
		// Exact prefix match.
		{"pg_hardstorage doctor", true},
		{"pg_hardstorage doctor -d db1", true},
		{"pg_hardstorage list db1", true},
		{"pg_hardstorage status", true},
		// Refused: not on allowlist.
		{"pg_hardstorage backup db1", false},
		{"pg_hardstorage restore db1 latest", false},
		{"rm -rf /", false},
		{"", false},
		// Prefix-but-not-word-boundary refusal.
		// "pg_hardstorage doctorate" doesn't start with
		// "pg_hardstorage doctor " (space-required).
		{"pg_hardstorage doctorate", false},
	}
	for _, tc := range cases {
		got := p.Allowed(tc.cmd)
		if got != tc.want {
			t.Errorf("Allowed(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

func TestSkillExecPolicy_EmptyAllowsNothing(t *testing.T) {
	var p safety.SkillExecPolicy
	if p.Allowed("pg_hardstorage doctor") {
		t.Error("empty AllowedExecutes should refuse all (skill author must opt in explicitly)")
	}
}

func TestContainsMutationFlag(t *testing.T) {
	cases := []struct {
		cmd      string
		wantFlag string
		wantHas  bool
	}{
		{"pg_hardstorage doctor", "", false},
		{"pg_hardstorage backup --apply", "--apply", true},
		{"pg_hardstorage kms shred --yes", "--yes", true},
		{"pg_hardstorage restore --force", "--force", true},
		{"pg_hardstorage restore --reset-chain-staging", "--reset-chain-staging", true},
		{"pg_hardstorage kms shred --confirm-keyring /etc/keyring", "--confirm-keyring", true},
		{"pg_hardstorage repo gc --apply --require-approval id-1", "--apply", true},
		// Equals form (--yes=true).
		{"pg_hardstorage kms shred --yes=true", "--yes", true},
		// Substring match should not fire (--yes-please isn't a refused flag).
		{"pg_hardstorage doctor --yes-please", "", false},
	}
	for _, tc := range cases {
		flag, has := safety.ContainsMutationFlag(tc.cmd)
		if has != tc.wantHas {
			t.Errorf("ContainsMutationFlag(%q) has=%v, want %v", tc.cmd, has, tc.wantHas)
		}
		if flag != tc.wantFlag {
			t.Errorf("ContainsMutationFlag(%q) flag=%q, want %q", tc.cmd, flag, tc.wantFlag)
		}
	}
}

func TestGate_ReadOnlyModeRefuses(t *testing.T) {
	d := safety.Gate(
		safety.ModeReadOnly,
		safety.SkillExecPolicy{AllowedExecutes: []string{"pg_hardstorage doctor"}},
		&safety.PreviewState{Commands: []string{"pg_hardstorage doctor"}},
		"pg_hardstorage doctor",
	)
	if d.Allowed {
		t.Errorf("read-only mode must refuse; got %+v", d)
	}
	if d.Reason != "read_only_mode" {
		t.Errorf("Reason = %q, want read_only_mode", d.Reason)
	}
}

func TestGate_FullChainAllows(t *testing.T) {
	policy := safety.SkillExecPolicy{
		AllowedExecutes: []string{"pg_hardstorage doctor"},
	}
	preview := &safety.PreviewState{
		Commands: []string{"pg_hardstorage doctor -d db1"},
	}
	d := safety.Gate(safety.ModeAdviseExecute, policy, preview,
		"pg_hardstorage doctor -d db1")
	if !d.Allowed {
		t.Errorf("full chain should allow; got %+v (reason=%s)", d, safety.Reason(d))
	}
	if d.MatchedPrefix != "pg_hardstorage doctor" {
		t.Errorf("MatchedPrefix = %q, want pg_hardstorage doctor", d.MatchedPrefix)
	}
}

func TestGate_SkillDisallowedRefuses(t *testing.T) {
	policy := safety.SkillExecPolicy{
		AllowedExecutes: []string{"pg_hardstorage doctor"},
	}
	preview := &safety.PreviewState{
		Commands: []string{"pg_hardstorage backup db1"},
	}
	d := safety.Gate(safety.ModeAdviseExecute, policy, preview,
		"pg_hardstorage backup db1")
	if d.Allowed || d.Reason != "skill_disallowed" {
		t.Errorf("skill_disallowed should fire; got %+v", d)
	}
}

func TestGate_MutationFlagRefuses(t *testing.T) {
	policy := safety.SkillExecPolicy{
		AllowedExecutes: []string{"pg_hardstorage repo gc"},
	}
	preview := &safety.PreviewState{
		Commands: []string{"pg_hardstorage repo gc --apply"},
	}
	d := safety.Gate(safety.ModeAdviseExecute, policy, preview,
		"pg_hardstorage repo gc --apply")
	if d.Allowed {
		t.Errorf("mutation flag should refuse; got %+v", d)
	}
	if d.Reason != "mutation_flag" || d.MutationFlag != "--apply" {
		t.Errorf("expected mutation_flag/--apply; got %+v", d)
	}
}

func TestGate_NoPreviewRefuses(t *testing.T) {
	policy := safety.SkillExecPolicy{
		AllowedExecutes: []string{"pg_hardstorage doctor"},
	}
	preview := &safety.PreviewState{
		Commands: []string{"pg_hardstorage doctor -d db1"}, // different command
	}
	d := safety.Gate(safety.ModeAdviseExecute, policy, preview,
		"pg_hardstorage doctor -d db2")
	if d.Allowed {
		t.Errorf("no_preview should refuse; got %+v", d)
	}
	if d.Reason != "no_preview" {
		t.Errorf("Reason = %q, want no_preview", d.Reason)
	}
}

func TestGate_EmptyCommandRefuses(t *testing.T) {
	d := safety.Gate(safety.ModeAdviseExecute,
		safety.SkillExecPolicy{AllowedExecutes: []string{"x"}},
		&safety.PreviewState{},
		"")
	if d.Allowed || d.Reason != "empty_command" {
		t.Errorf("empty_command should fire; got %+v", d)
	}
}

func TestGate_OrderingChecksReadOnlyFirst(t *testing.T) {
	// Even with everything else valid, read-only mode wins.
	d := safety.Gate(safety.ModeReadOnly,
		safety.SkillExecPolicy{AllowedExecutes: []string{"pg_hardstorage doctor"}},
		&safety.PreviewState{Commands: []string{"pg_hardstorage doctor"}},
		"pg_hardstorage doctor")
	if d.Allowed {
		t.Error("read-only must short-circuit")
	}
	if d.Reason != "read_only_mode" {
		t.Errorf("Reason = %q, want read_only_mode", d.Reason)
	}
}

func TestReason_NonEmptyForKnownReasons(t *testing.T) {
	for _, r := range []string{
		"read_only_mode",
		"skill_disallowed",
		"mutation_flag",
		"no_preview",
		"empty_command",
	} {
		got := safety.Reason(safety.GateDecision{Reason: r, MutationFlag: "--apply"})
		if got == "" || strings.Contains(got, "unknown") {
			t.Errorf("Reason(%q) = %q, want non-empty + non-unknown", r, got)
		}
	}
}
