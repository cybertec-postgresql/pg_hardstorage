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

// "journalctl gave us nothing" is two different situations, and the
// command must not conflate them.
//
// The original detection keyed on journalctl exiting 1 for "no
// entries". Real journalctl (systemd 255) exits 0 in BOTH of these:
//
//	journalctl -u systemd-journald.service --since -1us  -> exit 0
//	journalctl -u no-such-unit.service                   -> exit 0
//
// so the notfound.unit branch was unreachable in production and
// `logs --unit no-such-unit` returned success with no lines. This test
// used to plant a fake journalctl that exits 1 — the one behaviour
// journalctl does not exhibit — so it passed against a fiction and the
// dead branch kept its green tick.
//
// systemctl is what separates the cases, so both binaries are faked
// here: the test is about OUR logic, not about the host's unit list.
func fakeJournalAndSystemctl(t *testing.T, journal, systemctl string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if body == "" {
			return
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("journalctl", journal)
	write("systemctl", systemctl)
	t.Setenv("PATH", dir)
	return dir
}

// Unit systemd has never heard of: notfound, so the operator learns the
// name is wrong instead of staring at an empty log.
func TestLogs_NoEntries_UnknownUnit_IsNotFound(t *testing.T) {
	fakeJournalAndSystemctl(t,
		"#!/bin/sh\nexit 0\n",
		"#!/bin/sh\necho not-found\n")
	_, errb, exit := runCmd(t, "logs", "--output", "json")
	if exit != 6 {
		t.Errorf("unknown unit should exit 6 (NotFound); got %d\n%s", exit, errb)
	}
	if !strings.Contains(errb, "notfound.unit") {
		t.Errorf("expected notfound.unit code:\n%s", errb)
	}
}

// Unit exists and is simply quiet: NOT an error. Reporting notfound
// here would be worse than the silence it replaces — a healthy agent
// that has not logged recently would look like a missing one.
func TestLogs_NoEntries_QuietUnit_IsNotAnError(t *testing.T) {
	fakeJournalAndSystemctl(t,
		"#!/bin/sh\nexit 0\n",
		"#!/bin/sh\necho loaded\n")
	_, errb, exit := runCmd(t, "logs", "--output", "json")
	if exit != 0 {
		t.Errorf("a loaded-but-quiet unit must not be an error; got exit %d\n%s", exit, errb)
	}
}

// No systemctl to ask: stay silent rather than guess. Guessing is how
// the original bug happened.
func TestLogs_NoEntries_NoSystemctl_DoesNotGuess(t *testing.T) {
	fakeJournalAndSystemctl(t, "#!/bin/sh\nexit 0\n", "")
	_, errb, exit := runCmd(t, "logs", "--output", "json")
	if exit != 0 {
		t.Errorf("without systemctl the command must not claim notfound; got exit %d\n%s", exit, errb)
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
