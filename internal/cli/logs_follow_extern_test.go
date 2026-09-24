package cli_test

import (
	stdjson "encoding/json"
	"strings"
	"testing"
)

// `logs --follow` used to exec journalctl with inherited stdout whatever
// -o said, so `-o json --follow` printed journalctl's short-iso text into
// a stream scripts parse as JSON. json promises ONE document, which an
// endless tail cannot produce: refuse. ndjson streams: one event per
// journal entry.
func TestLogs_Follow_HonoursOutputFormat(t *testing.T) {
	fakeJournalAndSystemctl(t, `#!/bin/sh
for a in "$@"; do [ "$a" = json ] && j=1; done
if [ -n "$j" ]; then
  echo '{"MESSAGE":"hello","PRIORITY":"6","__REALTIME_TIMESTAMP":"1714291200000000"}'
else
  echo '2026-01-01T00:00:00+0000 host pg_hardstorage[1]: hello'
fi
`, "#!/bin/sh\necho loaded\n")

	out, errb, exit := runCmd(t, "logs", "db1", "--follow", "-o", "json")
	if exit != 2 || !strings.Contains(errb, "usage.follow_needs_stream") {
		t.Errorf("--follow -o json: exit %d, want 2 with usage.follow_needs_stream\nstdout: %s\nstderr: %s", exit, out, errb)
	}

	out, errb, exit = runCmd(t, "logs", "db1", "--follow", "-o", "ndjson")
	if exit != 0 {
		t.Fatalf("--follow -o ndjson: exit %d\n%s", exit, errb)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var ev struct {
		Component string         `json:"component"`
		Op        string         `json:"op"`
		Body      map[string]any `json:"body"`
	}
	if err := stdjson.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("--follow -o ndjson: first stdout line is not JSON: %v\n%s", err, out)
	}
	if ev.Component != "logs" || ev.Op != "line" || ev.Body["message"] != "hello" {
		t.Errorf("--follow -o ndjson: got %+v", ev)
	}
}
