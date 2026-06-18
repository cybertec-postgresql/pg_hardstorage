// Package logical orchestrates per-deployment logical-decoding
// pipelines. A pipeline is the tuple:
//
//	(deployment, stream-name, slot, plugin, publication, sink)
//
// The agent runs one pipeline per registered stream. v0.1 ships:
//
//   - Stream registry: a JSON state file under
//     paths.State()/logical_streams.json. `pg_hardstorage logical add`
//     appends; `logical list` walks it; `logical remove` deletes.
//
//   - The chunked sink (internal/logical/sinks/chunked) — CAS-backed,
//     per-batch manifests, idempotent on retry.
//
//   - Status reporting: walks committed segment manifests + reads PG
//     pg_replication_slots to compute lag.
//
// What's deferred to: Kafka / Pub/Sub / webhook / S3-events
// sinks, transform plugins (PII-redact), wal2json + pg_hardstorage_proto
// output plugins, AddOnCreate that creates the publication on the
// source PG (we require the operator to have created it).
package logical

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
)

// SchemaStream is the per-stream record schema.
const SchemaStream = "pg_hardstorage.logical_stream.v1"

// SchemaStateFile is the schema for the registry envelope.
const SchemaStateFile = "pg_hardstorage.logical_streams.v1"

// Stream is one configured logical-decoding pipeline.
type Stream struct {
	Name        string    `json:"name"`
	Deployment  string    `json:"deployment"`
	Slot        string    `json:"slot"`
	Plugin      string    `json:"plugin"`
	Publication string    `json:"publication"`
	SinkKind    string    `json:"sink_kind"` // "chunked" | "kafka" | ...
	RepoURL     string    `json:"repo_url"`
	CreatedAt   time.Time `json:"created_at"`
}

type stateFileBody struct {
	Schema  string   `json:"schema"`
	Streams []Stream `json:"streams"`
}

// Manager owns the registry's on-disk state file. Concurrency posture
// matches internal/standby and internal/timetravel: per-process
// serialisation; cross-process coordination is the operator's job.
type Manager struct {
	mu        sync.Mutex
	statePath string
}

// NewManager returns a manager backed by statePath.
func NewManager(statePath string) *Manager {
	return &Manager{statePath: statePath}
}

// AddOptions configures Add. The publication on the source PG must
// already exist; v0.1 doesn't create it (creating publications is a
// SQL operation against the source DB, which is the operator's
// concern at this slice).
type AddOptions struct {
	Name        string
	Deployment  string
	Slot        string
	Plugin      string
	Publication string
	SinkKind    string
	RepoURL     string
}

// Add appends a stream to the registry. Returns ErrAlreadyExists when
// the name is already registered.
func (m *Manager) Add(opts AddOptions) (*Stream, error) {
	if opts.Name == "" {
		return nil, errors.New("logical: Name is required")
	}
	if opts.Deployment == "" {
		return nil, errors.New("logical: Deployment is required")
	}
	if opts.Slot == "" {
		opts.Slot = "pg_hardstorage_logical_" + opts.Name
	}
	if opts.Plugin == "" {
		opts.Plugin = "pgoutput"
	}
	if opts.Publication == "" {
		return nil, errors.New("logical: Publication is required")
	}
	if opts.SinkKind == "" {
		opts.SinkKind = "chunked"
	}
	if opts.RepoURL == "" {
		return nil, errors.New("logical: RepoURL is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.loadStateLocked()
	if err != nil {
		return nil, err
	}
	for _, s := range state.Streams {
		if s.Name == opts.Name {
			return nil, fmt.Errorf("%w: %q", ErrAlreadyExists, opts.Name)
		}
	}
	stream := Stream{
		Name:        opts.Name,
		Deployment:  opts.Deployment,
		Slot:        opts.Slot,
		Plugin:      opts.Plugin,
		Publication: opts.Publication,
		SinkKind:    opts.SinkKind,
		RepoURL:     opts.RepoURL,
		CreatedAt:   time.Now().UTC(),
	}
	state.Streams = append(state.Streams, stream)
	if err := m.saveStateLocked(state); err != nil {
		return nil, err
	}
	return &stream, nil
}

// List returns every registered stream, sorted by name.
func (m *Manager) List() ([]Stream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.loadStateLocked()
	if err != nil {
		return nil, err
	}
	out := append([]Stream(nil), state.Streams...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns the named stream or ErrNotFound.
func (m *Manager) Get(name string) (*Stream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.loadStateLocked()
	if err != nil {
		return nil, err
	}
	for _, s := range state.Streams {
		if s.Name == name {
			return &s, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
}

// Remove deletes the named stream from the registry. The PG slot
// itself isn't dropped here — that requires a replication-mode
// connection to the source; the CLI command wires that in via a
// separate call to logicalreceiver.DropLogicalSlot.
func (m *Manager) Remove(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.loadStateLocked()
	if err != nil {
		return err
	}
	idx := -1
	for i, s := range state.Streams {
		if s.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	state.Streams = append(state.Streams[:idx], state.Streams[idx+1:]...)
	return m.saveStateLocked(state)
}

// ErrAlreadyExists / ErrNotFound mirror the standby / timetravel
// packages.
var (
	ErrAlreadyExists = errors.New("logical: name already exists")
	ErrNotFound      = errors.New("logical: stream not found")
)

// --- state file -------------------------------------------------------

func (m *Manager) loadStateLocked() (*stateFileBody, error) {
	body, err := os.ReadFile(m.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &stateFileBody{Schema: SchemaStateFile}, nil
		}
		return nil, fmt.Errorf("logical: read %s: %w", m.statePath, err)
	}
	var s stateFileBody
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("logical: parse %s: %w", m.statePath, err)
	}
	if s.Schema == "" {
		s.Schema = SchemaStateFile
	}
	if s.Schema != SchemaStateFile {
		return nil, fmt.Errorf("logical: state file schema %q is not supported; want %q", s.Schema, SchemaStateFile)
	}
	return &s, nil
}

func (m *Manager) saveStateLocked(s *stateFileBody) error {
	if s.Schema == "" {
		s.Schema = SchemaStateFile
	}
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if err := os.MkdirAll(filepath.Dir(m.statePath), 0o750); err != nil {
		return err
	}
	// fsutil.WriteFileAtomic: tmp+fsync+rename+syncDir.
	return fsutil.WriteFileAtomic(m.statePath, body, 0o600)
}
