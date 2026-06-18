// checkpoint.go — per-restore checkpoint file inside the target dir for resumable restores.
package restore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// CheckpointFilename is the name of the per-restore checkpoint file
// the restorer writes inside the target dir. The leading dot keeps
// it sorted before regular PG files in `ls` output and signals
// "internal bookkeeping" to the operator.
//
// Why inside the target dir rather than under a separate state
// directory: a half-restored target IS its own state. The operator
// inspecting `<target>/` sees the in-progress dir + a bookkeeping
// file together; both move (or get rm -rf'd) together.
const CheckpointFilename = ".pg_hardstorage_restore_state.json"

// SchemaCheckpoint is the JSON schema string. 24-month back-compat per
// the project-wide commitment.
const SchemaCheckpoint = "pg_hardstorage.restore_checkpoint.v1"

// Checkpoint is the persisted "what's been written so far" record. We
// track full-file completion only — partially-written files always
// re-fetch on resume. Per-chunk granularity would shrink resume cost
// at the price of a much larger checkpoint file; full-file is the
// SPEC's documented v0.1 trade-off.
type Checkpoint struct {
	Schema     string    `json:"schema"`
	BackupID   string    `json:"backup_id"`
	Deployment string    `json:"deployment"`
	TargetDir  string    `json:"target_dir"`
	StartedAt  time.Time `json:"started_at"`
	UpdatedAt  time.Time `json:"updated_at"`

	// CompletedFiles is the list of FileEntry.Path values that have
	// been fully materialised on disk. Sorted lex so the JSON
	// representation is deterministic and operator-readable diffs
	// across resume cycles produce a clean changelog.
	CompletedFiles []string `json:"completed_files"`

	// BytesWritten + ChunksFetched are running totals. Useful for
	// the resume-event body so an operator sees "X% already done"
	// before the restore continues.
	BytesWritten  int64 `json:"bytes_written"`
	ChunksFetched int   `json:"chunks_fetched"`
}

// CheckpointWriter is the in-flight checkpoint manager. It owns the
// path on disk and serialises updates so concurrent file-finish
// events don't corrupt the file.
//
// Concurrency: every public method takes the embedded mutex. Restore
// is single-goroutine in v0.1 so contention is negligible; we still
// take the lock so a future parallel-fetch refactor doesn't need to
// reshape this surface.
type CheckpointWriter struct {
	mu          sync.Mutex
	path        string
	cp          Checkpoint
	flushEvery  int64 // bytes between fsynced writes; 1 GiB default
	bytesAtLast int64
	dirty       bool
}

// NewCheckpointWriter constructs a writer rooted at targetDir. cp is
// the initial state — typically empty (fresh restore) or freshly-
// loaded (resume).
//
// flushEvery defaults to 1 GiB if zero. Tests can pin it to 1 byte
// for deterministic write-after-every-file behaviour.
func NewCheckpointWriter(targetDir string, cp Checkpoint, flushEvery int64) *CheckpointWriter {
	if cp.Schema == "" {
		cp.Schema = SchemaCheckpoint
	}
	if flushEvery <= 0 {
		flushEvery = 1 << 30 // 1 GiB
	}
	return &CheckpointWriter{
		path:       filepath.Join(targetDir, CheckpointFilename),
		cp:         cp,
		flushEvery: flushEvery,
	}
}

// MarkFileDone records that path is fully materialised. bytesWritten
// + chunksFetched are deltas (added to running totals). When the
// running byte total has advanced by flushEvery since the last
// flush, the checkpoint is written + fsynced.
//
// The on-disk file is updated via atomic tmp+rename so a crash
// mid-checkpoint never leaves the operator with a corrupt file —
// either the previous checkpoint or the new one.
func (w *CheckpointWriter) MarkFileDone(path string, bytesWritten int64, chunksFetched int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cp.CompletedFiles = append(w.cp.CompletedFiles, path)
	w.cp.BytesWritten += bytesWritten
	w.cp.ChunksFetched += chunksFetched
	w.cp.UpdatedAt = time.Now().UTC()
	w.dirty = true
	if w.cp.BytesWritten-w.bytesAtLast >= w.flushEvery {
		if err := w.flushLocked(); err != nil {
			return err
		}
		w.bytesAtLast = w.cp.BytesWritten
	}
	return nil
}

// Flush writes the current checkpoint to disk regardless of the
// flushEvery cadence. Called at restore-end (after every file is
// materialised) and at structured-shutdown points so the next
// resume sees the most recent state.
func (w *CheckpointWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.dirty {
		return nil
	}
	return w.flushLocked()
}

// Clear removes the on-disk checkpoint file. Called after a
// successful restore so the next restore into the same target dir
// starts from a clean slate. Idempotent — safe to call when the file
// doesn't exist.
func (w *CheckpointWriter) Clear() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := os.Remove(w.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checkpoint: remove %s: %w", w.path, err)
	}
	return nil
}

// State returns a snapshot of the in-memory checkpoint. Callers that
// want to surface progress to the operator (events, status JSON)
// read this rather than the on-disk file — the in-memory state is
// always at-or-ahead of the on-disk state.
func (w *CheckpointWriter) State() Checkpoint {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.cp
	out.CompletedFiles = append([]string(nil), w.cp.CompletedFiles...)
	return out
}

// flushLocked persists the current state via tmp+rename + fsync.
// mu must be held.
func (w *CheckpointWriter) flushLocked() error {
	// Sort the completed-files list for stable JSON output; the
	// in-memory list grows by append in iteration order, but the
	// file should round-trip deterministically.
	sort.Strings(w.cp.CompletedFiles)

	body, err := json.MarshalIndent(w.cp, "", "  ")
	if err != nil {
		return fmt.Errorf("checkpoint: marshal: %w", err)
	}
	body = append(body, '\n')

	tmp := w.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("checkpoint: open %s: %w", tmp, err)
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("checkpoint: write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("checkpoint: fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("checkpoint: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, w.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("checkpoint: rename %s -> %s: %w", tmp, w.path, err)
	}
	w.dirty = false
	return nil
}

// LoadCheckpoint reads the checkpoint file from targetDir. Returns
// (nil, nil) when no checkpoint exists — callers treat that as
// "fresh restore." Returns (cp, nil) when one exists; the returned
// Checkpoint has CompletedFiles ready for the resume path's
// "skip already-done" check.
func LoadCheckpoint(targetDir string) (*Checkpoint, error) {
	path := filepath.Join(targetDir, CheckpointFilename)
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("checkpoint: read %s: %w", path, err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(body, &cp); err != nil {
		return nil, fmt.Errorf("checkpoint: parse %s: %w", path, err)
	}
	if cp.Schema != SchemaCheckpoint {
		return nil, fmt.Errorf("checkpoint: schema %q not supported; want %q", cp.Schema, SchemaCheckpoint)
	}
	return &cp, nil
}

// CompletedSet returns the CompletedFiles slice as a set for O(1)
// "did we already write this?" checks during the resume path. The
// returned map is owned by the caller; mutations don't affect the
// underlying Checkpoint.
func (cp Checkpoint) CompletedSet() map[string]struct{} {
	out := make(map[string]struct{}, len(cp.CompletedFiles))
	for _, p := range cp.CompletedFiles {
		out[p] = struct{}{}
	}
	return out
}

// orResumeTime is a tiny helper used by Restore: when resuming, we
// want to keep the original StartedAt from the previous run so the
// audit log shows total wall-clock time, not just the resume slice.
// When fresh, we use the current run's startedAt.
func orResumeTime(resumed bool, cp *Checkpoint, fresh time.Time) time.Time {
	if resumed && cp != nil && !cp.StartedAt.IsZero() {
		return cp.StartedAt
	}
	return fresh
}

// completedSlice returns a copy of cp.CompletedFiles, or nil when cp
// is nil. Used to seed CheckpointWriter without aliasing the loaded
// slice (which the caller might still hold a reference to).
func completedSlice(cp *Checkpoint) []string {
	if cp == nil {
		return nil
	}
	return append([]string(nil), cp.CompletedFiles...)
}
