//go:build chaos

package topology

// Covers streamCoverage itself: it runs at the end of every soak, and
// a summary that silently reports zeroes would be worse than no
// summary at all — it would read as "this path was not exercised"
// when the truth is "the parser broke".

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A verbatim slice of a real stream.log, including the subject-line
// rendering the timeline is scraped from.
const sampleStreamLog = `17:16:09 [WARNING] wal.stream.reconnecting
  body: {"attempt": 139, "reason": "setup_failure"}
17:16:39 [INFO ] wal.timeline.history_captured  deployment=db1 timeline=29
17:16:39 [WARNING] wal.stream.start_behind_slot_restart_lsn
17:16:40 [INFO ] wal.timeline.streaming_historic  deployment=db1 timeline=27
17:16:41 [INFO ] wal.stream.starting  deployment=db1 timeline=27
17:17:02 [WARNING] wal.stream.reconnecting
17:17:03 [INFO ] wal.stream.starting  deployment=db1 timeline=28
`

func TestStreamCoverage_CountsPathsAndHighestTimeline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stream.log")
	if err := os.WriteFile(path, []byte(sampleStreamLog), 0o600); err != nil {
		t.Fatal(err)
	}
	got := streamCoverage(path)
	for _, want := range []string{
		"reconnects=2",
		"cross-timeline resumes=1",
		"histories captured=1",
		"behind-restart-lsn warnings=1",
		"recycled-WAL refusals=0",
		"max timeline=29",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
}

func TestStreamCoverage_MissingLogSaysSo(t *testing.T) {
	got := streamCoverage(filepath.Join(t.TempDir(), "absent.log"))
	if !strings.HasPrefix(got, "unavailable:") {
		t.Fatalf("got %q — a missing log must not read as a run that covered nothing", got)
	}
}
