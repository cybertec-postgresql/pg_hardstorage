package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// On hosts without journalctl on PATH (CI macOS runners, BSDs, some
// containers), the logs command must surface a structured error
// rather than a confusing "exec failed". Hide journalctl by
// scrubbing PATH to a known directory containing only this test's
// fixtures.
func TestLogs_NoJournalctl_StructuredError(t *testing.T) {
	dir := t.TempDir()
	// Some other binaries (sh, etc.) might be needed by go test
	// machinery, but the path lookup for `journalctl` specifically
	// will fail.
	t.Setenv("PATH", dir)
	_, errb, exit := runCmd(t, "logs", "--output", "json")
	if exit != 2 {
		t.Errorf("missing journalctl should exit 2 (Misuse); got %d", exit)
	}
	if !strings.Contains(errb, "usage.no_journalctl") {
		t.Errorf("expected usage.no_journalctl code:\n%s", errb)
	}
	if !strings.Contains(errb, "doesn't run systemd") {
		t.Errorf("error should mention systemd:\n%s", errb)
	}
}

// On hosts WITH a fake journalctl that returns nothing, the command
// surfaces a notfound.unit error — distinguishable from "no
// journalctl on host" so monitoring tools can pivot.
func TestLogs_NoEntries_NotFound(t *testing.T) {
	dir := t.TempDir()
	// Plant a fake journalctl that exits 1 (journalctl's "no
	// entries match this unit" exit). On Linux this works directly;
	// on darwin /bin/sh is at /bin/sh so we need a small wrapper.
	fake := filepath.Join(dir, "journalctl")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	_, errb, exit := runCmd(t, "logs", "--output", "json")
	if exit != 6 {
		t.Errorf("no entries should exit 6 (NotFound); got %d", exit)
	}
	if !strings.Contains(errb, "notfound.unit") {
		t.Errorf("expected notfound.unit code:\n%s", errb)
	}
}

// With a fake journalctl that emits one valid entry, the JSON output
// surfaces the parsed line — confirms the parseJournalJSON path
// works end-to-end.
func TestLogs_HappyPath_StructuredOutput(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "journalctl")
	// Emit two journal-shaped JSON objects.
	body := `#!/bin/sh
echo '{"MESSAGE":"hello","PRIORITY":"6","__REALTIME_TIMESTAMP":"1714291200000000"}'
echo '{"MESSAGE":"world","PRIORITY":"4","__REALTIME_TIMESTAMP":"1714291201000000"}'
`
	if err := os.WriteFile(fake, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	out, _, exit := runCmd(t, "logs", "db1", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d, out:\n%s", exit, out)
	}
	for _, want := range []string{
		`"unit": "pg_hardstorage@db1.service"`,
		`"message": "hello"`,
		`"message": "world"`,
		`"priority": "6"`,
		`"priority": "4"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
