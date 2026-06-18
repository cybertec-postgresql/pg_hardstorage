// Package standby manages hot-standby PostgreSQL instances fed by the
// backup pipeline.
//
// The user-visible primitive is `pg_hardstorage standby create db1
// --target /var/lib/pg/standby --repo s3://...` which:
//
//  1. Restores the latest committed backup of db1 into the target.
//  2. Writes `standby.signal` to keep PG in recovery indefinitely.
//  3. Writes `restore_command` in postgresql.auto.conf pointing at
//     our `wal fetch` shim so PG continuously pulls newly-archived
//     WAL from the same repo.
//  4. Records the standby in a state file so `standby list` can
//     report it and `standby destroy` can clean it up.
//
// What v0.1 deliberately does NOT do:
//
//   - Start the PG process. The standby is a configured data dir;
//     the operator brings PG up via systemd / pg_ctl / a Docker
//     container. The Result body emits the recommended invocation.
//   - Continuously stream WAL via primary_conninfo. The agent's
//     existing wal-stream plumbing puts WAL in the repo; the standby's
//     restore_command pulls from the repo. Streaming directly from
//     the primary is a enhancement when the standby co-locates
//     with the agent.
//   - Manage replica slot acknowledgements / hot_standby_feedback.
//     alongside the verifier subsystem's running-PG support.
package standby

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/walfetchcmd"
)

// SchemaStandby is the JSON schema string for standby state files.
// 24-month back-compat per the project-wide commitment.
const SchemaStandby = "pg_hardstorage.standby.v1"

// SchemaStateFile is the schema for the standbys.json state file.
const SchemaStateFile = "pg_hardstorage.standbys.v1"

// Standby is one configured-but-not-necessarily-running standby.
type Standby struct {
	Name       string    `json:"name"`
	Deployment string    `json:"deployment"`
	RepoURL    string    `json:"repo_url"`
	BackupID   string    `json:"backup_id"`
	TargetDir  string    `json:"target_dir"`
	PGVersion  int       `json:"pg_version"`
	CreatedAt  time.Time `json:"created_at"`
}

// stateFileBody is the persisted state file shape. We wrap entries in
// an envelope with an explicit Schema so future format changes can be
// detected and refused (or auto-migrated).
type stateFileBody struct {
	Schema   string    `json:"schema"`
	Standbys []Standby `json:"standbys"`
}

// Manager owns the on-disk state file. Concurrency: every public
// method is serialised through the embedded mutex; cross-process
// coordination is the operator's responsibility (run one
// `standby create` at a time per host).
type Manager struct {
	mu        sync.Mutex
	statePath string
	binPath   string
}

// NewManager returns a manager that persists to statePath. binPath is
// embedded into the standby's restore_command so even if the agent
// is rebuilt to a different location, the running standby keeps
// pulling WAL via the binary it was originally configured against.
func NewManager(statePath, binPath string) *Manager {
	return &Manager{statePath: statePath, binPath: binPath}
}

// CreateOptions configures Create. RepoURL is required; one of
// BackupID or "latest" identifies which backup to restore.
type CreateOptions struct {
	Name       string
	Deployment string
	RepoURL    string
	BackupID   string // "latest" or an explicit ID
	TargetDir  string

	// Verifier verifies the manifest signature. Required.
	Verifier *backup.Verifier

	// KEKForRef resolves the manifest's KEK reference. Required for
	// encrypted backups; ignored otherwise.
	KEKForRef func(ref string) ([encryption.KeyLen]byte, error)

	// UnwrapDEK unwraps a cloud-KMS-wrapped DEK server-side (issue #102);
	// required to build a standby from a backup wrapped with a cloud KMS
	// KEK. Forwarded to restore.Options.UnwrapDEK.
	UnwrapDEK func(ctx context.Context, kekRef string, wrapped []byte) ([]byte, error)

	// AllowOverwrite permits writing into a non-empty TargetDir. The
	// CLI gates this behind --force.
	AllowOverwrite bool
}

// Create restores the named backup into TargetDir, configures the
// standby files, and records the standby in the state file.
func (m *Manager) Create(ctx context.Context, opts CreateOptions) (*Standby, error) {
	if opts.Name == "" {
		return nil, errors.New("standby: Name is required")
	}
	if opts.Deployment == "" {
		return nil, errors.New("standby: Deployment is required")
	}
	if opts.RepoURL == "" {
		return nil, errors.New("standby: RepoURL is required")
	}
	if opts.TargetDir == "" {
		return nil, errors.New("standby: TargetDir is required")
	}
	if opts.Verifier == nil {
		return nil, errors.New("standby: Verifier is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	state, err := m.loadStateLocked()
	if err != nil {
		return nil, err
	}
	for _, s := range state.Standbys {
		if s.Name == opts.Name {
			return nil, fmt.Errorf("%w: %q", ErrAlreadyExists, opts.Name)
		}
	}

	backupID := opts.BackupID
	if backupID == "" {
		backupID = "latest"
	}

	// 1. Resolve "latest" via a manifest walk before kicking off the
	// restore. The standby record needs the concrete ID — and a real
	// "latest" at create time is more meaningful than the lazy
	// resolution PG would do via its own restore.
	resolvedID, pgVersion, err := m.resolveBackup(ctx, opts.RepoURL, opts.Deployment, backupID, opts.Verifier)
	if err != nil {
		return nil, err
	}

	// 2. Restore + write standby.signal + restore_command.
	//
	// walfetchcmd.Build wraps the underlying `wal fetch` invocation
	// in `sh -c` with POSIX-safe quoting and the exit-6 → exit-1
	// mapping PG needs at end-of-archive — see that package's
	// docstring for the full rationale.
	rcmd := walfetchcmd.Build(m.binPath, opts.Deployment, opts.RepoURL)

	if _, err := restore.Restore(ctx, restore.Options{
		RepoURL:        opts.RepoURL,
		Deployment:     opts.Deployment,
		BackupID:       resolvedID,
		TargetDir:      opts.TargetDir,
		Verifier:       opts.Verifier,
		KEKForRef:      opts.KEKForRef,
		UnwrapDEK:      opts.UnwrapDEK,
		AllowOverwrite: opts.AllowOverwrite,
		Recovery: &restore.Recovery{
			Enable:         true,
			StandbyMode:    true,
			RestoreCommand: rcmd,
			// "latest" follows whichever timeline the source PG is
			// on; a standby fed by the backup pipeline tracks
			// promotions transparently.
			Timeline: "latest",
		},
	}); err != nil {
		return nil, fmt.Errorf("standby: restore: %w", err)
	}

	// 3. Record + persist.
	s := Standby{
		Name:       opts.Name,
		Deployment: opts.Deployment,
		RepoURL:    opts.RepoURL,
		BackupID:   resolvedID,
		TargetDir:  opts.TargetDir,
		PGVersion:  pgVersion,
		CreatedAt:  time.Now().UTC(),
	}
	state.Standbys = append(state.Standbys, s)
	if err := m.saveStateLocked(state); err != nil {
		return nil, fmt.Errorf("standby: persist state: %w", err)
	}
	return &s, nil
}

// resolveBackup picks the concrete backup ID + reads PGVersion from
// the manifest. "latest" walks the deployment's manifests; otherwise
// we Read the named ID directly.
func (m *Manager) resolveBackup(ctx context.Context, repoURL, deployment, backupID string, verifier *backup.Verifier) (string, int, error) {
	rs, err := openManifestStore(ctx, repoURL)
	if err != nil {
		return "", 0, err
	}
	defer rs.Close()

	if backupID == "latest" {
		// Delegate to restore.ResolveLatest — the single source of
		// truth, which orders by the manifest's StoppedAt time. The
		// previous local loop took the lexicographic max of BackupID,
		// which is WRONG: an ID is "<dep>.<type>.<ts>.<seq>", so the
		// type segment (full / incremental_lsn / snapshot) is compared
		// BEFORE the timestamp. Because "full" < "incremental_lsn" <
		// "snapshot" lexically, an OLDER incremental_lsn outranked a
		// NEWER full and the standby seeded from the wrong (earlier)
		// backup — diverging from what `restore <dep> latest` picks.
		id, lerr := restore.ResolveLatest(ctx, rs.sp, deployment, verifier)
		if lerr != nil {
			if errors.Is(lerr, restore.ErrNoBackupsFound) {
				return "", 0, fmt.Errorf("standby: no committed backups for deployment %q", deployment)
			}
			return "", 0, fmt.Errorf("standby: resolve latest: %w", lerr)
		}
		backupID = id // read below for the PGVersion the caller needs
	}

	mm, err := rs.store.Read(ctx, deployment, backupID, verifier)
	if err != nil {
		return "", 0, fmt.Errorf("standby: read manifest: %w", err)
	}
	return mm.BackupID, mm.PGVersion, nil
}

// List returns every recorded standby, sorted by name.
func (m *Manager) List() ([]Standby, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.loadStateLocked()
	if err != nil {
		return nil, err
	}
	out := append([]Standby(nil), state.Standbys...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DestroyOptions tunes Destroy.
type DestroyOptions struct {
	// RemoveTargetDir, when true, also rm -rf's the data directory.
	// Default false: a destroy means "stop tracking this standby in
	// the state file" — leaving the data dir lets the operator
	// inspect it before manually wiping.
	RemoveTargetDir bool
}

// Destroy removes the named standby from the state file and (if
// opts.RemoveTargetDir) deletes the data directory.
func (m *Manager) Destroy(ctx context.Context, name string, opts DestroyOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, err := m.loadStateLocked()
	if err != nil {
		return err
	}
	idx := -1
	for i, s := range state.Standbys {
		if s.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	target := state.Standbys[idx].TargetDir
	state.Standbys = append(state.Standbys[:idx], state.Standbys[idx+1:]...)
	if err := m.saveStateLocked(state); err != nil {
		return err
	}
	if opts.RemoveTargetDir {
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("standby: remove target %s: %w", target, err)
		}
	}
	return nil
}

// ErrAlreadyExists is returned by Create when the name is already
// recorded.
var ErrAlreadyExists = errors.New("standby: name already exists")

// ErrNotFound is returned by Destroy when the name isn't in the
// state file.
var ErrNotFound = errors.New("standby: not found")

// --- state file --------------------------------------------------------

func (m *Manager) loadStateLocked() (*stateFileBody, error) {
	body, err := os.ReadFile(m.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &stateFileBody{Schema: SchemaStateFile}, nil
		}
		return nil, fmt.Errorf("standby: read %s: %w", m.statePath, err)
	}
	var s stateFileBody
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("standby: parse %s: %w", m.statePath, err)
	}
	if s.Schema == "" {
		s.Schema = SchemaStateFile
	}
	if s.Schema != SchemaStateFile {
		return nil, fmt.Errorf("standby: state file schema %q is not supported; want %q", s.Schema, SchemaStateFile)
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
	// fsutil.WriteFileAtomic handles the tmp+fsync+rename+syncDir
	// dance.  Without the parent-dir fsync the rename is visible to
	// in-memory readers but a power loss can erase it.
	return fsutil.WriteFileAtomic(m.statePath, body, 0o600)
}
