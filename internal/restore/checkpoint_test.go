package restore_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

func TestCheckpoint_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	w := restore.NewCheckpointWriter(dir, restore.Checkpoint{
		BackupID:   "db1.full.20260427T0900Z",
		Deployment: "db1",
		TargetDir:  dir,
		StartedAt:  time.Date(2026, 4, 27, 9, 0, 0, 0, time.UTC),
	}, 1) // flush after every byte

	if err := w.MarkFileDone("base/16384/2619", 8192, 2); err != nil {
		t.Fatal(err)
	}
	if err := w.MarkFileDone("global/pg_control", 8192, 1); err != nil {
		t.Fatal(err)
	}

	// Re-load and assert.
	cp, err := restore.LoadCheckpoint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cp == nil {
		t.Fatal("expected checkpoint to exist")
	}
	if cp.BackupID != "db1.full.20260427T0900Z" {
		t.Errorf("BackupID = %q", cp.BackupID)
	}
	if cp.BytesWritten != 16384 {
		t.Errorf("BytesWritten = %d, want 16384", cp.BytesWritten)
	}
	if cp.ChunksFetched != 3 {
		t.Errorf("ChunksFetched = %d, want 3", cp.ChunksFetched)
	}
	wantFiles := map[string]bool{
		"base/16384/2619":   false,
		"global/pg_control": false,
	}
	for _, p := range cp.CompletedFiles {
		wantFiles[p] = true
	}
	for p, found := range wantFiles {
		if !found {
			t.Errorf("CompletedFiles missing %q", p)
		}
	}

	// CompletedSet round-trip.
	set := cp.CompletedSet()
	if _, ok := set["base/16384/2619"]; !ok {
		t.Error("CompletedSet missing path")
	}
}

func TestCheckpoint_LoadAbsentReturnsNilNoError(t *testing.T) {
	dir := t.TempDir()
	cp, err := restore.LoadCheckpoint(dir)
	if err != nil {
		t.Fatalf("LoadCheckpoint(empty dir): %v", err)
	}
	if cp != nil {
		t.Errorf("LoadCheckpoint(empty dir) should return nil; got %+v", cp)
	}
}

func TestCheckpoint_Clear(t *testing.T) {
	dir := t.TempDir()
	w := restore.NewCheckpointWriter(dir, restore.Checkpoint{
		BackupID:   "x",
		Deployment: "db1",
	}, 1)
	if err := w.MarkFileDone("a", 1, 1); err != nil {
		t.Fatal(err)
	}

	// File exists.
	if _, err := restore.LoadCheckpoint(dir); err != nil {
		t.Fatal(err)
	}

	// Clear.
	if err := w.Clear(); err != nil {
		t.Fatal(err)
	}

	// Re-load → nil.
	cp, err := restore.LoadCheckpoint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cp != nil {
		t.Errorf("after Clear, LoadCheckpoint should return nil; got %+v", cp)
	}

	// Clear is idempotent.
	if err := w.Clear(); err != nil {
		t.Errorf("idempotent Clear should not error; got %v", err)
	}
}

func TestCheckpoint_LoadRejectsBadSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, restore.CheckpointFilename)
	body := []byte(`{"schema":"not.our.schema","backup_id":"x"}`)
	if err := writeAt(path, body); err != nil {
		t.Fatal(err)
	}
	_, err := restore.LoadCheckpoint(dir)
	if err == nil {
		t.Fatal("LoadCheckpoint should reject unknown schema")
	}
}

// TestCheckpoint_AtomicSwap simulates a partial flush by inspecting
// the tmp file's lifecycle: a flush that succeeds atomically swaps
// the .tmp into place. We can't easily inject a mid-write crash,
// but we CAN assert no .tmp is left behind after a clean flush.
func TestCheckpoint_NoLeftoverTmp(t *testing.T) {
	dir := t.TempDir()
	w := restore.NewCheckpointWriter(dir, restore.Checkpoint{
		BackupID:   "x",
		Deployment: "db1",
	}, 1)
	if err := w.MarkFileDone("a", 1, 1); err != nil {
		t.Fatal(err)
	}
	tmpPath := filepath.Join(dir, restore.CheckpointFilename+".tmp")
	if _, err := os.Stat(tmpPath); err == nil {
		t.Errorf("tmp file %s should have been renamed away", tmpPath)
	}
}

func TestCheckpoint_State(t *testing.T) {
	dir := t.TempDir()
	w := restore.NewCheckpointWriter(dir, restore.Checkpoint{
		BackupID:   "x",
		Deployment: "db1",
	}, 1<<30) // big flushEvery; State should still reflect in-memory
	if err := w.MarkFileDone("a", 100, 1); err != nil {
		t.Fatal(err)
	}
	if err := w.MarkFileDone("b", 200, 2); err != nil {
		t.Fatal(err)
	}
	st := w.State()
	if st.BytesWritten != 300 {
		t.Errorf("State.BytesWritten = %d, want 300", st.BytesWritten)
	}
	if len(st.CompletedFiles) != 2 {
		t.Errorf("State.CompletedFiles = %d, want 2", len(st.CompletedFiles))
	}
}
