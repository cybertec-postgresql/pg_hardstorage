package cli_test

import (
	"encoding/hex"
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/history"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// TestLlmHistory_KeyFileRoundTrip: a transcript recorded by
// `llm chat --history-key-file` (no local KEK) must be listable,
// readable and shreddable by passing the same key file to the
// history read commands.  Previously they had no such flag and
// always demanded a KEK-derived DEK, so those transcripts were
// unreachable.
func TestLlmHistory_KeyFileRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USER", "alice")

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	keyFile := filepath.Join(t.TempDir(), "history.key")
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Record a session exactly as openHistoryWriter does with
	// --history-key-file: the file's key IS the DEK.
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store, err := history.New(filepath.Join(p.State.Value, "llm", "conversations"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDEK(key); err != nil {
		t.Fatal(err)
	}
	w, err := store.Open(history.SessionMeta{Principal: "alice", Skill: "ask", StartedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(history.Entry{At: time.Now().UTC(), Role: "user", Op: "prompt", Body: stdjson.RawMessage(`"secret-question"`)}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(0); err != nil {
		t.Fatal(err)
	}
	sid := w.SessionID()

	stdout, stderr, exit := runCLI(t, "llm", "history", "list", "--history-key-file", keyFile, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("list exit=%d stderr=%s", exit, stderr)
	}
	if !strings.Contains(stdout, sid) {
		t.Errorf("list does not show session %s: %s", sid, stdout)
	}

	stdout, stderr, exit = runCLI(t, "llm", "history", "show", sid, "--history-key-file", keyFile, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("show exit=%d stderr=%s", exit, stderr)
	}
	if !strings.Contains(stdout, "secret-question") {
		t.Errorf("show did not decrypt the transcript: %s", stdout)
	}

	stdout, stderr, exit = runCLI(t, "llm", "history", "shred", "--yes", "--history-key-file", keyFile, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("shred exit=%d stderr=%s", exit, stderr)
	}
	if !strings.Contains(stdout, `"removed": 1`) {
		t.Errorf("shred did not remove the session: %s", stdout)
	}
}
