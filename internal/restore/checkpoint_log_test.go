package restore_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

func keysOf(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("0\x00base/16384/%d\nweird\"name", i)
	}
	return out
}

// The checkpoint JSON must stay small (the list lives in the append-only
// log), a resumed restore must see every key through a set that spills
// (4 in-memory entries for 50 keys), and a resumed writer must extend, not replace,
// the log, cutting off a torn tail.
func TestCheckpointLog_BoundedResume(t *testing.T) {
	dir := t.TempDir()
	keys := keysOf(50)
	w := restore.NewCheckpointWriter(dir, restore.Checkpoint{BackupID: "b", Deployment: "d"}, 1)
	for _, k := range keys {
		if err := w.MarkFileDone(k, 10, 1); err != nil {
			t.Fatal(err)
		}
	}
	state, _ := os.ReadFile(filepath.Join(dir, restore.CheckpointFilename))
	if strings.Contains(string(state), "base/16384") {
		t.Fatalf("checkpoint JSON still carries the file list:\n%s", state)
	}

	// Simulate a crash that tore an uncommitted record onto the log.
	logPath := filepath.Join(dir, restore.CompletedLogFilename)
	f, _ := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0)
	_, _ = f.WriteString(`"torn-uncomm`)
	_ = f.Close()

	cp, done, err := restore.LoadCompleted(dir, 4)
	if err != nil || cp == nil {
		t.Fatalf("LoadCompleted: %v %v", cp, err)
	}
	defer done.Close()
	if done.Len() != 50 || cp.BytesWritten != 500 || len(cp.CompletedFiles) != 0 {
		t.Fatalf("len=%d bytes=%d files=%d", done.Len(), cp.BytesWritten, len(cp.CompletedFiles))
	}
	for _, k := range keys {
		if !done.Has(k) {
			t.Fatalf("missing %q", k)
		}
	}
	if done.Has("0\x00nope") || done.Has("torn-uncomm") {
		t.Error("phantom key present")
	}

	w2, err := restore.ResumeCheckpointWriter(dir, *cp, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := w2.MarkFileDone("fresh", 1, 1); err != nil {
		t.Fatal(err)
	}
	full, err := restore.LoadCheckpoint(dir)
	if err != nil || len(full.CompletedFiles) != 51 || full.BytesWritten != 501 || full.CompletedCount != 51 {
		t.Fatalf("after resume: %v %+v", err, full)
	}
	if err := w2.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("Clear left the log behind: %v", err)
	}
}

// A checkpoint written by an earlier version (inline completed_files, no
// log) must still resume, and be migrated into the log on resume.
func TestCheckpointLog_LegacyInlineMigrates(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"schema":"pg_hardstorage.restore_checkpoint.v1","backup_id":"b","deployment":"d",` +
		`"started_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z",` +
		`"completed_files":["0\u0000a","0\u0000b","0\u0000c"],"bytes_written":30,"chunks_fetched":3}`
	if err := os.WriteFile(filepath.Join(dir, restore.CheckpointFilename), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cp, done, err := restore.LoadCompleted(dir, 0)
	if err != nil || done.Len() != 3 || !done.Has("0\x00b") {
		t.Fatalf("legacy load: %v len=%d", err, done.Len())
	}
	defer done.Close()
	w, err := restore.ResumeCheckpointWriter(dir, *cp, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.MarkFileDone("0\x00d", 10, 1); err != nil {
		t.Fatal(err)
	}
	got, err := restore.LoadCheckpoint(dir)
	if err != nil || strings.Join(got.CompletedFiles, ",") != "0\x00a,0\x00b,0\x00c,0\x00d" || got.BytesWritten != 40 {
		t.Fatalf("after migration: %v %+v", err, got)
	}
}

// A log shorter than the bytes the checkpoint committed means files were
// lost: refuse rather than silently re-trust a partial list.
func TestCheckpointLog_TruncatedLogIsRefused(t *testing.T) {
	dir := t.TempDir()
	w := restore.NewCheckpointWriter(dir, restore.Checkpoint{BackupID: "b"}, 1)
	_ = w.MarkFileDone("aaaa", 1, 1)
	_ = w.MarkFileDone("bbbb", 1, 1)
	if err := os.Truncate(filepath.Join(dir, restore.CompletedLogFilename), 5); err != nil {
		t.Fatal(err)
	}
	if _, _, err := restore.LoadCompleted(dir, 0); err == nil {
		t.Fatal("truncated log accepted")
	}
}
