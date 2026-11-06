package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
)

// docs/reference/error-codes.md tells automation to route on
// `splitbrain.*` — the signal that two clusters are archiving into one
// lineage. wal push reported every such refusal as `wal.push_failed`,
// with the splitbrain text only inside the message, so no rule keyed on
// the documented code could ever match. Found by checking documented
// codes against what the code actually emits.
//
// A real collision: archive a timeline history file, then push a
// DIFFERENT file under the same name.
func TestWalPushSplitBrainCarriesItsDocumentedCode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_ROOT", filepath.Join(dir, "root"))
	repo := "file://" + filepath.Join(dir, "repo")
	if _, stderr, exit := rotateCLI(t, "repo", "init", repo); exit != 0 {
		t.Fatalf("repo init: exit %d\n%s", exit, stderr)
	}
	write := func(sub, body string) string {
		p := filepath.Join(dir, sub, "00000002.history")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := write("a", "1\t0/3000000\tno recovery target specified\n")
	b := write("b", "1\t0/9000000\tsomething else entirely\n")

	if _, stderr, exit := rotateCLI(t, "wal", "push", "db1", a, "--repo", repo); exit != 0 {
		t.Fatalf("first push should archive: exit %d\n%s", exit, stderr)
	}
	stdout, stderr, exit := rotateCLI(t, "wal", "push", "db1", b, "--repo", repo, "-o", "json")
	out := stdout + stderr
	if exit == 0 {
		t.Fatal("pushing different content under an archived name must be refused")
	}
	if !strings.Contains(out, `"code": "splitbrain.content_mismatch"`) {
		t.Fatalf("split-brain must carry its documented code, got:\n%s", out)
	}
	if exit != 1 {
		t.Errorf("exit = %d; error-codes.md documents splitbrain.* as exit 1", exit)
	}
}

// The streaming sink raises the same refusals, and error-codes.md says
// splitbrain.* comes from "wal push and the streaming sink". The
// streamer stopped with wal.stream_permanent instead.
func TestWalStreamSplitBrainStopsWithItsDocumentedCode(t *testing.T) {
	for _, leaf := range []string{"content_mismatch", "system_identifier_mismatch", "read_failed"} {
		// As the streamer sees it: the sink's refusal, wrapped by the
		// replication loop.
		err := fmt.Errorf("replication: sink.OnRecord: %w", &walsink.SplitBrainError{
			Code: "splitbrain." + leaf,
			Err:  errors.New("segment already archived by another cluster"),
		})
		code, msg, stop := decideStreamStop(err, 0)
		if !stop {
			t.Errorf("%s: a split-brain must stop the streamer", leaf)
		}
		if code != "splitbrain."+leaf {
			t.Errorf("%s: stop code = %q, want splitbrain.%s", leaf, code, leaf)
		}
		if !strings.Contains(msg, "splitbrain."+leaf+": segment already archived") {
			t.Errorf("%s: message lost the refusal text: %q", leaf, msg)
		}
	}
}

// The code comes from the typed error, never from prose. It used to be
// scraped out of the message with a regular expression, so any error
// whose text merely contained "splitbrain.<word>" — a path, a quoted
// log line, a reworded message — produced a split-brain code, and a
// reworded real refusal lost it.
func TestSplitBrainCodeIsNotScrapedFromProse(t *testing.T) {
	for _, err := range []error{
		errors.New("open /var/lib/splitbrain.content_mismatch/segment: permission denied"),
		errors.New("see runbook R7 for splitbrain.read_failed handling"),
	} {
		if code := splitBrainCode(err); code != "" {
			t.Errorf("splitBrainCode(%q) = %q, want none: the text is not a refusal", err, code)
		}
		if isPermanentStreamError(err) {
			t.Errorf("%q stopped the streamer as a split-brain", err)
		}
	}
	if splitBrainCode(nil) != "" {
		t.Error("nil error has a split-brain code")
	}
}
