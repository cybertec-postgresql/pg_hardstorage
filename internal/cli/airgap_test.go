package cli_test

import (
	"os"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestAirgapFlag_FlipsPolicyForSubcommands: setting
// --airgapped on the root flips the process-wide policy
// before any subcommand RunE fires. We assert by running
// `version` (which never touches airgap directly) and then
// reading airgap.Default() — the flag's effect must outlive
// the command.
func TestAirgapFlag_FlipsPolicyForSubcommands(t *testing.T) {
	airgap.LockForTest(t)
	defer airgap.WithScope(airgap.Policy{Mode: airgap.ModeOff})()

	_, _, exit := runCLI(t, "--airgapped", "version")
	if exit != int(output.ExitOK) {
		t.Fatalf("--airgapped version exit=%d", exit)
	}
	if got := airgap.Default().Mode; got != airgap.ModeStrict {
		t.Errorf("after --airgapped: expected strict, got %v", got)
	}
}

// TestDoctor_AirgapSection_Off: when no airgap flag/env/config
// is set, the doctor reports mode=off and no sink refusals.
func TestDoctor_AirgapSection_Off(t *testing.T) {
	airgap.LockForTest(t)
	defer airgap.WithScope(airgap.Policy{Mode: airgap.ModeOff})()

	stdout, _, exit := runCLI(t, "doctor", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("doctor exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"airgap"`) || !strings.Contains(stdout, `"mode": "off"`) {
		t.Errorf("doctor JSON missing airgap.mode=off:\n%s", stdout)
	}
}

// TestDoctor_AirgapSection_StrictRefusesPublicSink:
// strict mode + a public-host sink reports the refusal as a
// warning issue. Operators see the misconfiguration before
// any event tries to flush.
func TestDoctor_AirgapSection_StrictRefusesPublicSink(t *testing.T) {
	airgap.LockForTest(t)
	defer airgap.WithScope(airgap.Policy{Mode: airgap.ModeStrict})()

	// We need a config file with a sink. Smallest sufficient
	// shape: drop a yaml under XDG_CONFIG_HOME pointing slack
	// at a public webhook. The runCLI helper provides a
	// per-test HOME.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")
	t.Setenv("XDG_DATA_HOME", home+"/.local/share")
	t.Setenv("XDG_CACHE_HOME", home+"/.cache")
	t.Setenv("XDG_STATE_HOME", home+"/.local/state")
	t.Setenv("XDG_RUNTIME_DIR", home+"/run")
	t.Setenv("PG_HARDSTORAGE_ROOT", "")

	cfgDir := home + "/.config/pg_hardstorage"
	if err := mkdirAll(cfgDir); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`schema: pg_hardstorage.config.v1
airgapped: strict
sinks:
  - name: ops-slack
    plugin: slack
    config:
      webhook_url: https://hooks.slack.com/services/T0/B0/secret
`)
	if err := writeFile(cfgDir+"/pg_hardstorage.yaml", cfg); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCLI(t, "doctor", "-o", "json")
	if exit != int(output.ExitOK) {
		// Doctor doesn't fail on issues by default; an exit
		// here is a real bug.
		t.Fatalf("doctor exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"mode": "strict"`) {
		t.Errorf("doctor JSON missing airgap.mode=strict:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"allowed": false`) {
		t.Errorf("doctor JSON should mark slack sink as not allowed:\n%s", stdout)
	}
	if !strings.Contains(stdout, "airgap.sink_refused") {
		t.Errorf("doctor JSON should emit issue airgap.sink_refused:\n%s", stdout)
	}
}

// helpers (small, file-local — avoids depending on the test
// fixtures of other CLI tests).

func mkdirAll(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}
