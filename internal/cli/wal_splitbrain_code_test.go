package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		err := errors.New("sink: splitbrain." + leaf + ": segment already archived by another cluster")
		code, _, stop := decideStreamStop(err, 0)
		if !stop {
			t.Errorf("%s: a split-brain must stop the streamer", leaf)
		}
		if code != "splitbrain."+leaf {
			t.Errorf("%s: stop code = %q, want splitbrain.%s", leaf, code, leaf)
		}
	}
}
