package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// configDir returns a fresh temp config dir wired into the env so
// loadEditableConfig picks it up.
func configDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", dir)
	return dir
}

func TestNotify_AddListRemove_RoundTrip(t *testing.T) {
	dir := configDir(t)

	// Add via slack plugin (registered by side-effect import).
	_, _, exit := runCmd(t,
		"notify", "add", "slack",
		"--name", "ops",
		"--set", "webhook_url=https://hooks.slack.com/services/T/B/X",
		"--min-severity", "warning",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("add exit = %d", exit)
	}

	// File should exist with the sink block.
	body, err := os.ReadFile(filepath.Join(dir, "pg_hardstorage.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"name: ops",
		"plugin: slack",
		"webhook_url: https://hooks.slack.com/services/T/B/X",
		"min_severity: warning",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("config missing %q\n%s", want, body)
		}
	}

	// list reflects what's there, redacting the webhook secret.
	out, _, exit := runCmd(t, "notify", "list", "--output", "json")
	if exit != 0 {
		t.Fatalf("list exit = %d", exit)
	}
	if !strings.Contains(out, `"name": "ops"`) {
		t.Errorf("list missing the sink:\n%s", out)
	}
	if strings.Contains(out, "T/B/X") {
		t.Errorf("list leaked the secret webhook path:\n%s", out)
	}
	if !strings.Contains(out, `"endpoint": "https://hooks.slack.com/****"`) {
		t.Errorf("list should redact the webhook path; got:\n%s", out)
	}

	// remove deletes it.
	_, _, exit = runCmd(t, "notify", "remove", "ops", "--output", "json")
	if exit != 0 {
		t.Fatalf("remove exit = %d", exit)
	}
	body, _ = os.ReadFile(filepath.Join(dir, "pg_hardstorage.yaml"))
	if strings.Contains(string(body), "name: ops") {
		t.Errorf("remove didn't delete the sink:\n%s", body)
	}
}

func TestNotify_Add_RejectsInvalidConfig(t *testing.T) {
	configDir(t)
	// Slack requires webhook_url; add without it must error.
	_, stderr, exit := runCmd(t,
		"notify", "add", "slack",
		"--output", "json",
	)
	if exit == 0 {
		t.Fatal("expected non-zero exit on missing webhook_url")
	}
	if !strings.Contains(stderr, "webhook_url") {
		t.Errorf("error should mention webhook_url; got:\n%s", stderr)
	}
}

func TestNotify_Add_ReplaceRequiresYes(t *testing.T) {
	configDir(t)
	add := func(extra ...string) (int, string) {
		args := append([]string{
			"notify", "add", "slack",
			"--name", "ops",
			"--set", "webhook_url=https://hooks.slack.com/services/T/B/X",
			"--output", "json",
		}, extra...)
		out, _, exit := runCmd(t, args...)
		return exit, out
	}
	if exit, _ := add(); exit != 0 {
		t.Fatalf("first add exit = %d", exit)
	}
	exit, _ := add()
	if exit != 7 { // ExitConflict
		t.Errorf("second add (no --yes) should exit 7 (Conflict); got %d", exit)
	}
	if exit, _ := add("--yes"); exit != 0 {
		t.Errorf("second add with --yes should succeed; got exit %d", exit)
	}
}

func TestNotify_Remove_NonExistent_NotFound(t *testing.T) {
	configDir(t)
	_, _, exit := runCmd(t, "notify", "remove", "ghost", "--output", "json")
	if exit != 6 {
		t.Errorf("remove of missing sink should exit 6 (NotFound); got %d", exit)
	}
}

func TestNotify_AddListRemove_BadKVRejected(t *testing.T) {
	configDir(t)
	_, _, exit := runCmd(t,
		"notify", "add", "slack",
		"--set", "no-equals-sign",
		"--output", "json",
	)
	if exit != 2 {
		t.Errorf("malformed --set should exit 2 (Misuse); got %d", exit)
	}
}
