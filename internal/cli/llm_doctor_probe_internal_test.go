package cli

import (
	"slices"
	"testing"
)

// TestLlmDoctorProbeArgs_ForwardsProviderFlags: the round-trip probe
// re-execs `llm ask`; it must carry the operator's --provider /
// --endpoint / --model or the child probes a different provider than
// the one doctor just reported on (previously it dropped all three).
func TestLlmDoctorProbeArgs_ForwardsProviderFlags(t *testing.T) {
	args := llmDoctorProbeArgs("ollama", "http://127.0.0.1:11434", "llama3")
	for _, want := range [][2]string{
		{"--provider", "ollama"},
		{"--endpoint", "http://127.0.0.1:11434"},
		{"--model", "llama3"},
	} {
		i := slices.Index(args, want[0])
		if i < 0 || i+1 >= len(args) || args[i+1] != want[1] {
			t.Errorf("probe args %q missing %s %s", args, want[0], want[1])
		}
	}
	if args[0] != "llm" || args[1] != "ask" {
		t.Errorf("probe args must start with `llm ask`: %q", args)
	}

	// Unset flags are left out so the child resolves env/config
	// exactly as the parent did.
	bare := llmDoctorProbeArgs("", "", "")
	for _, f := range []string{"--provider", "--endpoint", "--model"} {
		if slices.Contains(bare, f) {
			t.Errorf("empty %s must not be forwarded: %q", f, bare)
		}
	}
}
