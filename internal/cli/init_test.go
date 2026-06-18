package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

func TestParseSchedExpr(t *testing.T) {
	cases := []struct {
		in   string
		want config.ScheduleSpec
	}{
		{"", config.ScheduleSpec{}},
		{"off", config.ScheduleSpec{}},
		{"OFF", config.ScheduleSpec{}},
		{"  off  ", config.ScheduleSpec{}},
		{"every 6h", config.ScheduleSpec{Every: "6h"}},
		{"EVERY 12h", config.ScheduleSpec{Every: "12h"}},
		{"daily_at 04:00", config.ScheduleSpec{DailyAt: "04:00"}},
		{"6h", config.ScheduleSpec{Every: "6h"}}, // bare duration → Every
	}
	for _, c := range cases {
		got := parseSchedExpr(c.in)
		if got != c.want {
			t.Errorf("parseSchedExpr(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestPrompter_AutoAcceptUsesDefault(t *testing.T) {
	var out bytes.Buffer
	p := newPrompter(strings.NewReader(""), &out, true)
	got, err := p.askLine("foo", "bar", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "bar" {
		t.Errorf("got %q, want bar (default)", got)
	}
	if out.Len() != 0 {
		t.Errorf("autoAccept should not write any prompt; got %q", out.String())
	}
}

func TestPrompter_AutoAcceptRejectsEmptyDefault(t *testing.T) {
	var out bytes.Buffer
	p := newPrompter(strings.NewReader(""), &out, true)
	_, err := p.askLine("foo", "", validateNonEmpty)
	if err == nil {
		t.Fatal("expected error: required value missing in --yes mode")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error should mention required; got %v", err)
	}
}

func TestPrompter_InteractiveTakesInput(t *testing.T) {
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("supplied\n"), &out, false)
	got, err := p.askLine("foo", "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "supplied" {
		t.Errorf("got %q, want supplied", got)
	}
}

func TestPrompter_InteractiveFallsBackToDefault(t *testing.T) {
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("\n"), &out, false)
	got, err := p.askLine("foo", "thedefault", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "thedefault" {
		t.Errorf("got %q, want thedefault", got)
	}
}

func TestPrompter_InteractiveRetriesOnValidationFail(t *testing.T) {
	var out bytes.Buffer
	// First line empty (rejected by validateNonEmpty), second line valid.
	p := newPrompter(strings.NewReader("\nok\n"), &out, false)
	got, err := p.askLine("foo", "", validateNonEmpty)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ok" {
		t.Errorf("got %q, want ok", got)
	}
	// The retry path should have produced two prompts.
	if strings.Count(out.String(), "?") < 2 {
		t.Errorf("expected at least two prompts; got %q", out.String())
	}
}

func TestPrompter_AskYes(t *testing.T) {
	cases := []struct {
		input string
		dflt  bool
		want  bool
	}{
		{"y\n", false, true},
		{"yes\n", false, true},
		{"Y\n", false, true},
		{"\n", true, true},   // default yes
		{"\n", false, false}, // default no
		{"n\n", true, false},
		{"random\n", true, false}, // unrecognised → false
	}
	for _, c := range cases {
		var out bytes.Buffer
		p := newPrompter(strings.NewReader(c.input), &out, false)
		got := p.askYes("foo", c.dflt)
		if got != c.want {
			t.Errorf("askYes input=%q dflt=%v: got %v, want %v", c.input, c.dflt, got, c.want)
		}
	}
}

func TestValidateNonEmpty(t *testing.T) {
	if err := validateNonEmpty(""); err == nil {
		t.Error("empty must reject")
	}
	if err := validateNonEmpty("  \t  "); err == nil {
		t.Error("whitespace-only must reject")
	}
	if err := validateNonEmpty("ok"); err != nil {
		t.Errorf("non-empty should pass; got %v", err)
	}
}

func TestShapeFirstBackup_Nil(t *testing.T) {
	if got := shapeFirstBackup(nil); got != nil {
		t.Errorf("nil input should produce nil output; got %+v", got)
	}
}

func TestInitResultBody_WriteText(t *testing.T) {
	body := initResultBody{
		Deployment:  "db1",
		PGVersion:   17,
		SystemID:    "7388123",
		Timeline:    1,
		RepoURL:     "file:///var/lib/pg_hardstorage/repo",
		ConfigPath:  "/etc/pg_hardstorage/pg_hardstorage.yaml",
		KeyringPath: "/etc/pg_hardstorage/keyring",
		FirstBackup: &firstBackupSummary{
			BackupID:     "db1.full.20260428T1200Z",
			LogicalBytes: 12 * 1024 * 1024 * 1024,
			DurationMS:   8472,
		},
	}
	var sb strings.Builder
	if err := body.WriteText(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"pg_hardstorage initialized",
		"db1",
		"PostgreSQL:   17",
		"7388123",
		"file:///var/lib/pg_hardstorage/repo",
		"db1.full.20260428T1200Z",
		"Next steps",
		"pg_hardstorage agent",
		"pg_hardstorage wal stream",
		"pg_hardstorage doctor",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestInitResultBody_WriteText_NoFirstBackup(t *testing.T) {
	body := initResultBody{
		Deployment:  "db1",
		SystemID:    "7388123",
		RepoURL:     "file:///x",
		ConfigPath:  "/c.yaml",
		KeyringPath: "/k",
	}
	var sb strings.Builder
	if err := body.WriteText(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if strings.Contains(out, "First backup") {
		t.Errorf("no first-backup section should appear when nil; got:\n%s", out)
	}
	if !strings.Contains(out, "Next steps") {
		t.Errorf("Next steps should still appear; got:\n%s", out)
	}
}

// guard against the import-pruner removing what the wizard
// transitively depends on.
var _ = errors.New
