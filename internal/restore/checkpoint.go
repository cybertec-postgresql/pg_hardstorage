// checkpoint.go — per-restore checkpoint file inside the target dir for resumable restores.
package restore

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
	"io"
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

// CompletedLogFilename is the append-only companion of the checkpoint: one
// JSON-encoded string (a completed file's checkpoint key) per line. Keeping
// the completed set here instead of in the checkpoint's completed_files array
// means neither the writer nor a resuming restore ever holds, sorts or
// re-marshals the whole list, which grows with the FILE count (millions of
// files on a 100 TB cluster). The checkpoint records how many log bytes are
// committed (CompletedLogBytes); anything past that is a torn tail from a
// crash and is ignored and overwritten. Checkpoints written by earlier
// versions carry the list inline in completed_files and are still read.
const CompletedLogFilename = ".pg_hardstorage_restore_done.log"

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
	//
	// Only legacy checkpoint files and the LoadCheckpoint / State helpers
	// (tests, tooling) populate it; the restorer's own state is the
	// completed-files log, summarised by CompletedLogBytes and CompletedCount.
	CompletedFiles []string `json:"completed_files"`

	// CompletedLogBytes is the committed length of CompletedLogFilename.
	CompletedLogBytes int64 `json:"completed_log_bytes,omitempty"`
	// CompletedCount is how many files the log (or legacy list) holds.
	CompletedCount int `json:"completed_count,omitempty"`

	// legacyInline marks a checkpoint read from a pre-log file whose
	// completed_files array was still inline.
	legacyInline bool

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
	logErrV     error // first log-append failure; surfaced by the next MarkFileDone / Flush

	// Append-only completed-files log (see CompletedLogFilename). Opened
	// lazily on the first append; logBytes counts bytes handed to logw.
	logPath  string
	log      *os.File
	logw     *bufio.Writer
	logBytes int64
	resume   bool // log is extended (truncated to CompletedLogBytes), not restarted
	legacy   bool // state on disk still carries the inline list; migrate it first
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
	w := &CheckpointWriter{
		path:       filepath.Join(targetDir, CheckpointFilename),
		logPath:    filepath.Join(targetDir, CompletedLogFilename),
		cp:         cp,
		flushEvery: flushEvery,
	}
	// A seed list (tests, tools) starts a fresh log; a writer that extends
	// an existing checkpoint comes from ResumeCheckpointWriter instead.
	seed := w.cp.CompletedFiles
	w.cp.CompletedFiles = nil
	w.cp.CompletedLogBytes, w.cp.CompletedCount = 0, 0
	for _, p := range seed {
		if err := w.appendLocked(p); err != nil {
			w.logErr(err)
		}
	}
	return w
}

// ResumeCheckpointWriter returns a writer that extends the checkpoint hdr
// (from ScanCheckpoint) in place: new completions are appended to the
// existing log after cutting off any torn tail. A legacy checkpoint's inline
// list is migrated into the log first, streamed, never held.
func ResumeCheckpointWriter(targetDir string, hdr Checkpoint, flushEvery int64) (*CheckpointWriter, error) {
	legacy := hdr.legacyInline
	hdr.CompletedFiles = nil
	w := NewCheckpointWriter(targetDir, hdr, flushEvery)
	w.cp.CompletedLogBytes, w.cp.CompletedCount = hdr.CompletedLogBytes, hdr.CompletedCount
	w.resume = true
	if legacy {
		// Re-stream the inline list into a fresh log. Until the next flush
		// the on-disk state is still the legacy file, which ignores the log.
		w.resume = false
		w.cp.CompletedLogBytes, w.cp.CompletedCount = 0, 0
		if _, err := scanState(w.path, func(key string, _ bool) error { return w.appendLocked(key) }); err != nil {
			return nil, err
		}
		w.dirty = true
	}
	if w.logErrV != nil {
		return nil, w.logErrV
	}
	return w, nil
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
	if err := w.appendLocked(path); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
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
	if w.log != nil {
		_ = w.log.Close()
		w.log, w.logw = nil, nil
	}
	if err := os.Remove(w.logPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checkpoint: remove %s: %w", w.logPath, err)
	}
	return nil
}

// State returns a snapshot of the in-memory checkpoint. Callers that
// want to surface progress to the operator (events, status JSON)
// read this rather than the on-disk file — the in-memory state is
// always at-or-ahead of the on-disk state. CompletedFiles is read back
// from the log, so this is O(files): for tests and small restores, not
// the 100 TB hot path.
func (w *CheckpointWriter) State() Checkpoint {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.cp
	out.CompletedFiles = nil
	if w.logw != nil {
		_ = w.logw.Flush()
	}
	if w.log != nil {
		_ = readLog(w.logPath, w.logBytes, func(k string) error {
			out.CompletedFiles = append(out.CompletedFiles, k)
			return nil
		})
	}
	return out
}

func (w *CheckpointWriter) logErr(err error) {
	if w.logErrV == nil {
		w.logErrV = err
	}
}

// appendLocked adds key to the completed-files log. mu must be held (or the
// writer not yet shared).
func (w *CheckpointWriter) appendLocked(key string) error {
	if w.logErrV != nil {
		return w.logErrV
	}
	if w.log == nil {
		flags := os.O_WRONLY | os.O_CREATE
		if !w.resume {
			flags |= os.O_TRUNC
		}
		f, err := os.OpenFile(w.logPath, flags, 0o600)
		if err != nil {
			return fmt.Errorf("open %s: %w", w.logPath, err)
		}
		if w.resume {
			// Drop the torn tail past the committed length, then append.
			if err := f.Truncate(w.cp.CompletedLogBytes); err != nil {
				_ = f.Close()
				return fmt.Errorf("truncate %s: %w", w.logPath, err)
			}
		}
		if _, err := f.Seek(w.cp.CompletedLogBytes, io.SeekStart); err != nil {
			_ = f.Close()
			return fmt.Errorf("seek %s: %w", w.logPath, err)
		}
		w.log, w.logw, w.logBytes = f, bufio.NewWriterSize(f, 64<<10), w.cp.CompletedLogBytes
	}
	line, err := json.Marshal(key)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	n, err := w.logw.Write(line)
	w.logBytes += int64(n)
	if err != nil {
		w.logErrV = fmt.Errorf("append %s: %w", w.logPath, err)
		return w.logErrV
	}
	w.cp.CompletedCount++
	return nil
}

// flushLocked persists the current state via tmp+rename + fsync: first the
// completed-files log (data before the checkpoint that vouches for it), then
// the small checkpoint JSON. mu must be held.
func (w *CheckpointWriter) flushLocked() error {
	if w.logErrV != nil {
		return fmt.Errorf("checkpoint: %w", w.logErrV)
	}
	if w.log != nil {
		if err := w.logw.Flush(); err != nil {
			return fmt.Errorf("checkpoint: write %s: %w", w.logPath, err)
		}
		if err := w.log.Sync(); err != nil {
			return fmt.Errorf("checkpoint: fsync %s: %w", w.logPath, err)
		}
		w.cp.CompletedLogBytes = w.logBytes
	}
	out := w.cp
	out.CompletedFiles = []string{} // the list lives in the log
	body, err := json.MarshalIndent(out, "", "  ")
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
	// fsync the parent: f.Sync() above flushed the checkpoint's data
	// and inode, but not the dentry the rename created. Without this
	// a power loss can take the rename with it and resume reads a
	// checkpoint older than the files already on disk.
	if err := fsutil.SyncDir(filepath.Dir(w.path)); err != nil {
		return fmt.Errorf("checkpoint: fsync parent of %s: %w", w.path, err)
	}
	w.dirty = false
	return nil
}

// LoadCheckpoint reads the checkpoint file from targetDir. Returns
// (nil, nil) when no checkpoint exists — callers treat that as
// "fresh restore." Returns (cp, nil) when one exists; the returned
// Checkpoint has CompletedFiles populated, sorted. That list is O(files)
// in memory: the restorer itself uses ScanCheckpoint / LoadCompleted and
// never calls this.
func LoadCheckpoint(targetDir string) (*Checkpoint, error) {
	var keys []string
	cp, err := ScanCheckpoint(targetDir, func(k string) error {
		keys = append(keys, k)
		return nil
	})
	if cp == nil || err != nil {
		return cp, err
	}
	sort.Strings(keys)
	cp.CompletedFiles = keys
	return cp, nil
}

// ScanCheckpoint reads the checkpoint in targetDir, calling fn once for
// every completed-file key (inline legacy list first, then the committed
// part of the log) without ever holding the list. The returned Checkpoint
// has no CompletedFiles. (nil, nil) means no checkpoint exists.
func ScanCheckpoint(targetDir string, fn func(key string) error) (*Checkpoint, error) {
	path := filepath.Join(targetDir, CheckpointFilename)
	cp, err := scanState(path, func(k string, _ bool) error { return fn(k) })
	if cp == nil || err != nil {
		return cp, err
	}
	n := 0
	if cp.CompletedLogBytes > 0 {
		if err := readLog(filepath.Join(targetDir, CompletedLogFilename), cp.CompletedLogBytes, func(k string) error {
			n++
			return fn(k)
		}); err != nil {
			return nil, err
		}
	}
	if cp.legacyInline {
		return cp, nil
	}
	cp.CompletedCount = n
	return cp, nil
}

// scanState stream-decodes the checkpoint JSON, calling fn for each entry of
// an inline completed_files array. It returns nil, nil when the file is absent.
func scanState(path string, fn func(key string, legacy bool) error) (*Checkpoint, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("checkpoint: read %s: %w", path, err)
	}
	defer f.Close()
	parseErr := func(err error) error { return fmt.Errorf("checkpoint: parse %s: %w", path, err) }
	cp := &Checkpoint{}
	dec := json.NewDecoder(bufio.NewReader(f))
	if tok, err := dec.Token(); err != nil {
		return nil, parseErr(err)
	} else if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, parseErr(errors.New("not a JSON object"))
	}
	fields := map[string]any{
		"schema": &cp.Schema, "backup_id": &cp.BackupID, "deployment": &cp.Deployment,
		"target_dir": &cp.TargetDir, "started_at": &cp.StartedAt, "updated_at": &cp.UpdatedAt,
		"bytes_written": &cp.BytesWritten, "chunks_fetched": &cp.ChunksFetched,
		"completed_log_bytes": &cp.CompletedLogBytes, "completed_count": &cp.CompletedCount,
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, parseErr(err)
		}
		key, _ := kt.(string)
		if key == "completed_files" {
			open, err := dec.Token()
			if err != nil {
				return nil, parseErr(err)
			}
			if open == nil {
				continue
			}
			if d, ok := open.(json.Delim); !ok || d != '[' {
				return nil, parseErr(errors.New("completed_files is not an array"))
			}
			n := 0
			for dec.More() {
				var k string
				if err := dec.Decode(&k); err != nil {
					return nil, parseErr(err)
				}
				if cp.Schema != SchemaCheckpoint {
					continue // rejected below; do not feed a foreign document's keys on
				}
				n++
				if err := fn(k, true); err != nil {
					return nil, err
				}
			}
			if n > 0 {
				cp.legacyInline = true
				cp.CompletedCount = n
			}
			if _, err := dec.Token(); err != nil {
				return nil, parseErr(err)
			}
			continue
		}
		dst, known := fields[key]
		if !known {
			dst = new(json.RawMessage)
		}
		if err := dec.Decode(dst); err != nil {
			return nil, parseErr(err)
		}
	}
	if cp.Schema != SchemaCheckpoint {
		return nil, fmt.Errorf("checkpoint: schema %q not supported; want %q", cp.Schema, SchemaCheckpoint)
	}
	return cp, nil
}

// readLog replays the first limit bytes of the completed-files log.
func readLog(path string, limit int64, fn func(key string) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("checkpoint: read %s: %w (the checkpoint expects %d committed bytes; remove the target directory's checkpoint files to start over)", path, err, limit)
	}
	defer f.Close()
	br := bufio.NewReaderSize(io.LimitReader(f, limit), 64<<10)
	var read int64
	for {
		line, err := br.ReadBytes('\n')
		read += int64(len(line))
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("checkpoint: read %s: %w", path, err)
		}
		if len(line) > 0 {
			var k string
			if jerr := json.Unmarshal(bytes.TrimSuffix(line, []byte("\n")), &k); jerr != nil || !bytes.HasSuffix(line, []byte("\n")) {
				return fmt.Errorf("checkpoint: %s: corrupt record at byte %d", path, read-int64(len(line)))
			}
			if err := fn(k); err != nil {
				return err
			}
		}
		if err != nil {
			break
		}
	}
	if read < limit {
		return fmt.Errorf("checkpoint: %s holds %d bytes but the checkpoint committed %d", path, read, limit)
	}
	return nil
}

// CompletedKeys is the bounded-memory set of files a resumed restore has
// already materialised: a spill.HashSet of SHA-256(key), so heap use is
// capped no matter how many files the checkpoint lists.
type CompletedKeys struct{ set *spill.HashSet }

func keyHash(key string) [32]byte { return sha256.Sum256([]byte(key)) }

// Has reports whether key was recorded complete. A nil set is empty.
func (c *CompletedKeys) Has(key string) bool {
	return c != nil && c.set.Has(keyHash(key))
}

// Len is the number of distinct recorded files.
func (c *CompletedKeys) Len() int {
	if c == nil {
		return 0
	}
	return c.set.Len()
}

// Err reports a spill I/O failure; Has answers "present" when one occurs, so
// callers must check it before trusting a hit.
func (c *CompletedKeys) Err() error {
	if c == nil {
		return nil
	}
	return c.set.Err()
}

// Close removes the set's spill files.
func (c *CompletedKeys) Close() error {
	if c == nil {
		return nil
	}
	return c.set.Close()
}

// LoadCompleted reads targetDir's checkpoint into a bounded CompletedKeys
// set (memEntries = 0 means spill.DefaultMemEntries). It returns the
// checkpoint header (no CompletedFiles) for ResumeCheckpointWriter, or
// (nil, nil, nil) when there is no checkpoint.
func LoadCompleted(targetDir string, memEntries int) (*Checkpoint, *CompletedKeys, error) {
	ck := &CompletedKeys{set: spill.NewHashSet(spill.Options{MemEntries: memEntries})}
	cp, err := ScanCheckpoint(targetDir, func(k string) error {
		ck.set.Add(keyHash(k))
		return ck.set.Err()
	})
	if err != nil || cp == nil {
		_ = ck.Close()
		return nil, nil, err
	}
	return cp, ck, nil
}

// CompletedSet returns the CompletedFiles slice as a set for O(1)
// "did we already write this?" checks. The returned map is owned by the
// caller. O(files) memory: the restorer uses CompletedKeys instead.
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
