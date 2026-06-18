package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config/configcheck"
)

// TestRenderConfigFindings_EndToEnd pins the wired Layer-4b composition: a
// model answer that hand-edits pg_hardstorage.yaml with an INVENTED key
// (`backup_schedule:` instead of the nested `schedule.backup.every`) produces
// a config-validation warning block under the answer, with a did-you-mean.
func TestRenderConfigFindings_EndToEnd(t *testing.T) {
	answer := "Edit your config:\n\n```yaml\n" +
		"deployments:\n" +
		"  prod:\n" +
		"    backup_schedule: \"every 6h\"\n" +
		"```\n"
	var buf bytes.Buffer
	renderConfigFindings(&buf, configcheck.Scrub(answer))
	out := buf.String()
	if !strings.Contains(out, "config-validation warnings") {
		t.Fatalf("expected a config-validation warning block, got:\n%s", out)
	}
	if !strings.Contains(out, `unknown key "backup_schedule" under deployments.*`) {
		t.Errorf("warning should name the bad key and its path:\n%s", out)
	}
	if !strings.Contains(out, `did you mean "schedule"`) {
		t.Errorf("warning should suggest the real key:\n%s", out)
	}
}

// TestRenderConfigFindings_CleanIsSilent: a correct nested config produces no
// warning block — the validator must not be noise on valid YAML.
func TestRenderConfigFindings_CleanIsSilent(t *testing.T) {
	answer := "```yaml\n" +
		"deployments:\n  prod:\n    schedule:\n      backup:\n        every: 6h\n```\n"
	var buf bytes.Buffer
	renderConfigFindings(&buf, configcheck.Scrub(answer))
	if buf.Len() != 0 {
		t.Errorf("valid config should render nothing, got:\n%s", buf.String())
	}
}

// TestRenderConfigFindings_ValueShapes pins the round-19 value-shape block:
// a one-of violation, a type/shape mismatch, and a bad enum each render with a
// clear message.
func TestRenderConfigFindings_ValueShapes(t *testing.T) {
	answer := "```yaml\ndeployments:\n  prod:\n    schedule:\n      backup:\n" +
		"        every: 6h\n        daily_at: \"02:00\"\n" +
		"    retention:\n      policy: weekly\n```\n"
	var buf bytes.Buffer
	renderConfigFindings(&buf, configcheck.Scrub(answer))
	out := buf.String()
	for _, want := range []string{
		"set at most one of every / daily_at / at", // one_of
		"not a valid policy",                       // enum
		"gfs | simple | count",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered block missing %q:\n%s", want, out)
		}
	}
}
