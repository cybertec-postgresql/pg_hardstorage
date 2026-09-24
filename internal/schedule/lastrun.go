// lastrun.go — persistence of per-task last-run times across agent restarts.
package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LastRunStore remembers when each task (by Task.Name) last ran. The
// engine reads it once per task in Add and writes it after every run.
type LastRunStore interface {
	// LastRun returns the recorded start of the task's last run, and
	// false when there is no record.
	LastRun(name string) (time.Time, bool)
	// RecordRun records that the task started a run at t.
	RecordRun(name string, t time.Time)
}

// lastRunSchema tags the on-disk file so a future format change is
// detected instead of misread.
const lastRunSchema = "pg_hardstorage.schedule.lastrun.v1"

type lastRunFile struct {
	Schema string               `json:"schema"`
	Tasks  map[string]time.Time `json:"tasks"`
}

// FileLastRunStore is a LastRunStore backed by one small JSON file,
// rewritten atomically (temp file + rename) on every RecordRun so a
// crash mid-write leaves the previous record intact.
type FileLastRunStore struct {
	path    string
	onError func(error)

	mu    sync.Mutex
	tasks map[string]time.Time
}

// OpenFileLastRunStore loads the store at path. A missing file is an
// empty store. A file that cannot be read or decoded is returned as an
// error ALONGSIDE a usable empty store: the caller reports it, and the
// agent keeps scheduling -- an unreadable record must not block
// backups, and an empty record errs toward running `every` tasks now,
// not later. onError receives RecordRun write failures (nil ignores
// them); the engine has no event channel of its own.
func OpenFileLastRunStore(path string, onError func(error)) (*FileLastRunStore, error) {
	s := &FileLastRunStore{path: path, onError: onError, tasks: map[string]time.Time{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return s, fmt.Errorf("schedule: read last-run state %s: %w", path, err)
	}
	var f lastRunFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return s, fmt.Errorf("schedule: decode last-run state %s: %w", path, err)
	}
	if f.Schema != lastRunSchema {
		return s, fmt.Errorf("schedule: last-run state %s has schema %q, want %q", path, f.Schema, lastRunSchema)
	}
	for k, v := range f.Tasks {
		s.tasks[k] = v
	}
	return s, nil
}

// LastRun implements LastRunStore.
func (s *FileLastRunStore) LastRun(name string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[name]
	return t, ok
}

// RecordRun implements LastRunStore.
func (s *FileLastRunStore) RecordRun(name string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[name] = t.UTC()
	if err := s.flushLocked(); err != nil && s.onError != nil {
		s.onError(err)
	}
}

func (s *FileLastRunStore) flushLocked() error {
	body, err := json.MarshalIndent(lastRunFile{Schema: lastRunSchema, Tasks: s.tasks}, "", "  ")
	if err != nil {
		return fmt.Errorf("schedule: encode last-run state: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("schedule: create last-run state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".lastrun-*.json")
	if err != nil {
		return fmt.Errorf("schedule: write last-run state: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("schedule: write last-run state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("schedule: write last-run state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("schedule: write last-run state: %w", err)
	}
	return nil
}
