package configcheck_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config/configcheck"
)

// TestScrub_FlagsInventedScheduleKeys is the motivating case (round-18 F02):
// the model told the operator to add flat `backup_schedule:`/`rotate_schedule:`
// keys under a deployment; the real schema nests cadence under
// `schedule.backup.every`. Those keys must be flagged.
func TestScrub_FlagsInventedScheduleKeys(t *testing.T) {
	text := "Edit your config:\n\n```yaml\n" +
		"deployments:\n" +
		"  prod:\n" +
		"    backup_schedule: \"every 6h\"\n" +
		"    rotate_schedule: \"daily_at 04:00\"\n" +
		"```\n"
	fs := configcheck.Scrub(text)
	got := map[string]string{}
	for _, f := range fs {
		got[f.Key] = f.Path
	}
	for _, k := range []string{"backup_schedule", "rotate_schedule"} {
		if _, ok := got[k]; !ok {
			t.Errorf("expected %q to be flagged; findings=%+v", k, fs)
		}
	}
	// The path should point under a deployment (wildcard).
	for _, f := range fs {
		if f.Path != "deployments.*" {
			t.Errorf("path = %q, want deployments.*", f.Path)
		}
		// did-you-mean should surface the real `schedule` key.
		if f.Suggestion != "schedule" {
			t.Errorf("suggestion for %q = %q, want schedule", f.Key, f.Suggestion)
		}
	}
}

// TestScrub_ValidConfigClean: the REAL nested schema must produce no findings.
func TestScrub_ValidConfigClean(t *testing.T) {
	text := "```yaml\n" +
		"schema: pg_hardstorage.config.v1\n" +
		"deployments:\n" +
		"  prod:\n" +
		"    pg_connection: \"host=db port=5432\"\n" +
		"    repo: file:///srv/repo\n" +
		"    schedule:\n" +
		"      backup:\n" +
		"        every: 6h\n" +
		"      rotate:\n" +
		"        daily_at: \"04:00\"\n" +
		"```\n"
	if fs := configcheck.Scrub(text); len(fs) != 0 {
		t.Errorf("valid nested config should be clean, got: %+v", fs)
	}
}

// TestScrub_FlagsNestedUnknownKey: an invented key DEEP in the schema
// (under schedule) is caught with the right path.
func TestScrub_FlagsNestedUnknownKey(t *testing.T) {
	text := "```yaml\n" +
		"deployments:\n" +
		"  prod:\n" +
		"    schedule:\n" +
		"      backup:\n" +
		"        cron: \"0 2 * * *\"\n" + // ScheduleSpec has every/daily_at/at, not cron
		"```\n"
	fs := configcheck.Scrub(text)
	if len(fs) != 1 || fs[0].Key != "cron" || fs[0].Path != "deployments.*.schedule.backup" {
		t.Fatalf("want one finding cron@deployments.*.schedule.backup, got: %+v", fs)
	}
}

// TestScrub_SkipsNonConfigBlocks: a bare fragment (no deployments/schema
// marker) and non-YAML blocks are skipped — no false positives.
func TestScrub_SkipsNonConfigBlocks(t *testing.T) {
	cases := []string{
		// bare schedule fragment shown out of context — not rooted at config
		"```yaml\nschedule:\n  backup:\n    every: 6h\n```\n",
		// a bash block
		"```bash\npg_hardstorage backup prod --repo r\n```\n",
		// unrelated YAML (k8s-ish)
		"```yaml\napiVersion: v1\nkind: Pod\nmetadata:\n  name: x\n```\n",
	}
	for _, c := range cases {
		if fs := configcheck.Scrub(c); len(fs) != 0 {
			t.Errorf("non-config block should be skipped, got: %+v\nblock=%s", fs, c)
		}
	}
}

// TestScrub_DeploymentAndSinkNamesAreWildcards: map keys (deployment names,
// here a made-up deployment name) are never flagged — only struct fields are.
func TestScrub_DeploymentNamesNotFlagged(t *testing.T) {
	text := "```yaml\ndeployments:\n  any-weird-name-123:\n    repo: file:///r\n```\n"
	if fs := configcheck.Scrub(text); len(fs) != 0 {
		t.Errorf("deployment name (map key) must not be flagged, got: %+v", fs)
	}
}

// TestScrub_FlagsBogusTopLevelKey: an invented top-level config key is caught
// at the root.
func TestScrub_FlagsBogusTopLevelKey(t *testing.T) {
	text := "```yaml\nschema: pg_hardstorage.config.v1\nencryption_key: abc123\n```\n"
	fs := configcheck.Scrub(text)
	if len(fs) != 1 || fs[0].Key != "encryption_key" || fs[0].Path != "" {
		t.Fatalf("want encryption_key flagged at root, got: %+v", fs)
	}
}

// TestScrub_OneOfScheduleSpec: a schedule task with BOTH every and daily_at
// violates the at-most-one rule (the round-19 value-shape example).
func TestScrub_OneOfScheduleSpec(t *testing.T) {
	text := "```yaml\ndeployments:\n  prod:\n    schedule:\n      backup:\n" +
		"        every: 6h\n        daily_at: \"02:00\"\n```\n"
	fs := configcheck.Scrub(text)
	var oneof *configcheck.Finding
	for i := range fs {
		if fs[i].Kind == configcheck.KindOneOf {
			oneof = &fs[i]
		}
	}
	if oneof == nil {
		t.Fatalf("expected a one_of finding, got: %+v", fs)
	}
	if oneof.Path != "deployments.*.schedule.backup" {
		t.Errorf("path = %q, want deployments.*.schedule.backup", oneof.Path)
	}
	if !strings.Contains(oneof.Message, "every") || !strings.Contains(oneof.Message, "daily_at") {
		t.Errorf("message should name the conflicting keys: %q", oneof.Message)
	}
}

// TestScrub_ShapeMismatch: a struct field given a scalar (the flat-vs-nested
// confusion in VALUE form), and a numeric field given a non-numeric string.
func TestScrub_ShapeMismatch(t *testing.T) {
	cases := []struct {
		name, yaml, wantKey, wantPath string
	}{
		{"struct-as-scalar",
			"deployments:\n  prod:\n    schedule: \"every 6h\"\n", "schedule", "deployments.*"},
		{"scalar-as-block",
			"deployments:\n  prod:\n    repo:\n      url: x\n", "repo", "deployments.*"},
		{"int-as-words",
			"deployments:\n  prod:\n    retention:\n      keep_daily: six\n", "keep_daily", "deployments.*.retention"},
	}
	for _, c := range cases {
		fs := configcheck.Scrub("```yaml\n" + c.yaml + "```\n")
		var got *configcheck.Finding
		for i := range fs {
			if fs[i].Kind == configcheck.KindType && fs[i].Key == c.wantKey {
				got = &fs[i]
			}
		}
		if got == nil {
			t.Errorf("%s: expected a type finding for %q, got: %+v", c.name, c.wantKey, fs)
			continue
		}
		if got.Path != c.wantPath {
			t.Errorf("%s: path = %q, want %q", c.name, got.Path, c.wantPath)
		}
	}
}

// TestScrub_EnumRetentionPolicy: an unknown retention policy is flagged; a
// valid one is silent.
func TestScrub_EnumRetentionPolicy(t *testing.T) {
	bad := "```yaml\ndeployments:\n  prod:\n    retention:\n      policy: weekly\n```\n"
	fs := configcheck.Scrub(bad)
	if len(fs) != 1 || fs[0].Kind != configcheck.KindEnum || fs[0].Key != "policy" {
		t.Fatalf("want one enum finding for policy, got: %+v", fs)
	}
	if !strings.Contains(fs[0].Message, "gfs") {
		t.Errorf("enum message should list allowed values: %q", fs[0].Message)
	}
	good := "```yaml\ndeployments:\n  prod:\n    retention:\n      policy: GFS\n```\n" // case-insensitive
	if fs := configcheck.Scrub(good); len(fs) != 0 {
		t.Errorf("valid policy (any case) should be clean, got: %+v", fs)
	}
}

// TestScrub_ValueShapeNoFalsePositiveOnValid: a fully-correct config with a
// single schedule key, list residency, int retention counts stays silent.
func TestScrub_ValueShapeNoFalsePositiveOnValid(t *testing.T) {
	text := "```yaml\nschema: pg_hardstorage.config.v1\ndeployments:\n  prod:\n" +
		"    repo: file:///r\n    residency: [eu, us]\n" +
		"    schedule:\n      backup:\n        every: 6h\n" +
		"    retention:\n      policy: count\n      keep_fulls: 3\n```\n"
	if fs := configcheck.Scrub(text); len(fs) != 0 {
		t.Errorf("valid config must stay silent, got: %+v", fs)
	}
}
