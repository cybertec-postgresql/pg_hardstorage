package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestNotify_Add_RepeatedSetKeyBuildsList: `--set to=a --set to=b`
// must persist both recipients.  Previously --set was a
// StringSlice (so a comma inside a value split it into bogus
// key=value pairs) and the last value per key silently won, so an
// email sink could only ever notify one address.
func TestNotify_Add_RepeatedSetKeyBuildsList(t *testing.T) {
	dir := configDir(t)

	_, stderr, exit := runCmd(t,
		"notify", "add", "email",
		"--name", "mail",
		"--set", "smtp_host=smtp.example.com",
		"--set", "auth=none",
		"--set", "from=pgh@example.com",
		"--set", "to=a@example.com",
		"--set", "to=b@example.com",
		"--set", "subject_prefix=[pgh, prod]",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("add exit = %d stderr=%s", exit, stderr)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "pg_hardstorage.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Sinks []struct {
			Name   string         `yaml:"name"`
			Config map[string]any `yaml:"config"`
		} `yaml:"sinks"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse config: %v\n%s", err, raw)
	}
	if len(doc.Sinks) != 1 {
		t.Fatalf("want 1 sink, got %d\n%s", len(doc.Sinks), raw)
	}
	cfg := doc.Sinks[0].Config
	to, ok := cfg["to"].([]any)
	if !ok || len(to) != 2 || to[0] != "a@example.com" || to[1] != "b@example.com" {
		t.Errorf("to = %#v, want [a@example.com b@example.com]\n%s", cfg["to"], raw)
	}
	// A comma inside a value is part of the value, not a separator.
	if got := cfg["subject_prefix"]; got != "[pgh, prod]" {
		t.Errorf("subject_prefix = %#v, want \"[pgh, prod]\"", got)
	}
	// A key given once stays a scalar.
	if got := cfg["smtp_host"]; got != "smtp.example.com" {
		t.Errorf("smtp_host = %#v, want scalar string", got)
	}
}

// TestNotify_List_RedactsCredentialsInURL: `notify list` is meant to
// be pasteable into a ticket.  Credentials can sit in the userinfo
// (https://user:pass@host) or in the query string (?token=...), and a
// URL with no path used to be returned verbatim.
func TestNotify_List_RedactsCredentialsInURL(t *testing.T) {
	configDir(t)
	for _, tc := range []struct{ name, url string }{
		{"userinfo", "https://alice:hunter2@hooks.example.com"},
		{"userinfo-path", "https://alice:hunter2@hooks.example.com/hook"},
		{"query", "https://hooks.example.com?token=hunter2&channel=ops"},
		{"query-path", "https://hooks.example.com/hook?api_key=hunter2&sig=hunter2"},
	} {
		_, stderr, exit := runCmd(t, "notify", "add", "webhook",
			"--name", tc.name, "--set", "url="+tc.url, "--output", "json")
		if exit != 0 {
			t.Fatalf("add %s: exit=%d stderr=%s", tc.name, exit, stderr)
		}
	}
	out, _, exit := runCmd(t, "notify", "list", "--output", "json")
	if exit != 0 {
		t.Fatalf("list exit = %d", exit)
	}
	for _, leak := range []string{"hunter2", "alice"} {
		if strings.Contains(out, leak) {
			t.Errorf("notify list leaked %q:\n%s", leak, out)
		}
	}
	// The host stays visible, and a non-secret query key survives
	// so the operator can still tell sinks apart.
	if !strings.Contains(out, "hooks.example.com") {
		t.Errorf("host should remain visible:\n%s", out)
	}
	if !strings.Contains(out, "channel=ops") {
		t.Errorf("non-sensitive query param should remain visible:\n%s", out)
	}
}
