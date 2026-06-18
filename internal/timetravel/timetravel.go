// Package timetravel manages ephemeral read-only PostgreSQL instances
// restored to a specific historical point.
//
// The user-visible primitive is `pg_hardstorage timetravel create db1
// --at "2026-04-01T00:00:00Z" --target /var/tmp/db1@apr1` which:
//
//  1. Resolves the named target (RFC3339 / natural language /
//     LSN / backup-id) to a concrete recovery target.
//  2. Restores the backup containing that LSN into the target dir.
//  3. Writes recovery.signal + restore_command + recovery_target_*
//     so PG enters recovery, replays WAL up to the target, and pauses
//     (the SPEC's recommended action — keeps the cluster readable
//     without it auto-promoting and accidentally diverging from
//     production).
//  4. Records the session in a state file with a TTL so a forgotten
//     session is auto-reclaimable via `timetravel cleanup`.
//
// Timetravel vs standby: a standby follows production indefinitely;
// a timetravel session is pinned at a moment and self-expires. They
// share the restore + recovery plumbing under internal/restore but
// keep their own state files because their lifecycles diverge.
//
// What v0.1 deliberately does NOT do:
//
//   - Start the PG process. alongside the verifier subsystem's
//     running-PG support. The Result body emits the recommended
//     `pg_ctl -D <target> -o '-p <port>' start` invocation.
//   - Auto-tear-down via a daemon. `timetravel cleanup` is a manual
//     sweep the operator runs (or wires into cron); a daemon
//     would need the supervisor's exposed lifecycle which lands in
//     .
package timetravel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/naturaltime"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/walfetchcmd"
)

// SchemaSession is the JSON schema string for one Session record.
const SchemaSession = "pg_hardstorage.timetravel.v1"

// SchemaStateFile is the schema for the state file envelope.
const SchemaStateFile = "pg_hardstorage.timetravel_sessions.v1"

// DefaultTTL applies when CreateOptions.TTL is zero. One hour is short
// enough that a forgotten timetravel session doesn't quietly hold
// gigabytes of disk for days, long enough for a real forensic query
// session.
const DefaultTTL = time.Hour

// Session is one ephemeral timetravel instance.
type Session struct {
	Name       string    `json:"name"`
	Deployment string    `json:"deployment"`
	RepoURL    string    `json:"repo_url"`
	BackupID   string    `json:"backup_id"`
	TargetDir  string    `json:"target_dir"`
	TargetSpec string    `json:"target_spec"` // raw --at value
	TargetTime time.Time `json:"target_time,omitempty"`
	TargetLSN  string    `json:"target_lsn,omitempty"`
	PGVersion  int       `json:"pg_version"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// IsExpired reports whether the session's TTL has elapsed.
func (s Session) IsExpired() bool {
	return !s.ExpiresAt.IsZero() && time.Now().After(s.ExpiresAt)
}

// stateFileBody is the persisted state file shape.
type stateFileBody struct {
	Schema   string    `json:"schema"`
	Sessions []Session `json:"sessions"`
}

// Manager owns the on-disk state file. Same posture as standby's
// Manager: per-process serialisation via mu; cross-process
// coordination is "run one at a time per host."
type Manager struct {
	mu        sync.Mutex
	statePath string
	binPath   string
}

// NewManager returns a manager backed by statePath. binPath is
// embedded into the session's restore_command.
func NewManager(statePath, binPath string) *Manager {
	return &Manager{statePath: statePath, binPath: binPath}
}

// CreateOptions configures Create.
type CreateOptions struct {
	Name       string
	Deployment string
	RepoURL    string
	TargetDir  string

	// At is the raw --at value. One of: RFC3339 timestamp, natural
	// time ("5 minutes ago"), an LSN ("0/3F5A1B40"), or "latest".
	At string

	// TTL controls how long the session is considered active before
	// `timetravel cleanup` will reap it. Zero → DefaultTTL.
	TTL time.Duration

	// Verifier verifies manifest signatures. Required.
	Verifier *backup.Verifier

	// KEKForRef resolves manifest KEK references. Required for
	// encrypted backups.
	KEKForRef func(ref string) ([encryption.KeyLen]byte, error)

	// UnwrapDEK unwraps a cloud-KMS-wrapped DEK server-side (issue #102);
	// required to time-travel a backup wrapped with a cloud KMS KEK.
	// Forwarded to restore.Options.UnwrapDEK.
	UnwrapDEK func(ctx context.Context, kekRef string, wrapped []byte) ([]byte, error)

	// AllowOverwrite permits writing into a non-empty TargetDir.
	AllowOverwrite bool

	// Now overrides the wall-clock for natural-time parsing. Tests
	// pin this; production leaves it zero (we use time.Now()).
	Now time.Time
}

// Create restores the named backup with PITR up to the resolved
// target, configures recovery files (action=pause), and records the
// session.
func (m *Manager) Create(ctx context.Context, opts CreateOptions) (*Session, error) {
	if opts.Name == "" {
		return nil, errors.New("timetravel: Name is required")
	}
	if opts.Deployment == "" {
		return nil, errors.New("timetravel: Deployment is required")
	}
	if opts.RepoURL == "" {
		return nil, errors.New("timetravel: RepoURL is required")
	}
	if opts.TargetDir == "" {
		return nil, errors.New("timetravel: TargetDir is required")
	}
	if opts.At == "" {
		return nil, errors.New("timetravel: At is required (RFC3339, natural time, or LSN)")
	}
	if opts.Verifier == nil {
		return nil, errors.New("timetravel: Verifier is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	state, err := m.loadStateLocked()
	if err != nil {
		return nil, err
	}
	for _, s := range state.Sessions {
		if s.Name == opts.Name {
			return nil, fmt.Errorf("%w: %q", ErrAlreadyExists, opts.Name)
		}
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Resolve the target into either a TargetTime or TargetLSN.
	atTime, atLSN, err := resolveTarget(opts.At, now)
	if err != nil {
		return nil, err
	}

	// Pick the backup that contains the target LSN. v0.1 uses the
	// "latest backup whose stop time / stop_lsn is at-or-before the
	// target" rule — the standard PITR walker. To keep this self-
	// contained and avoid duplicating the planner from
	// internal/restore, we reuse it via the Options below; the
	// Restore call resolves the right base backup automatically when
	// given a Recovery target plus deployment.
	//
	// In practice the existing Restore wants an explicit BackupID.
	// We resolve it ourselves: walk manifests, pick the latest with
	// StoppedAt <= target.

	pickedID, pgVersion, err := m.pickBackupForTarget(ctx, opts.RepoURL, opts.Deployment, atTime, opts.Verifier)
	if err != nil {
		return nil, err
	}

	// walfetchcmd.Build wraps the inner `wal fetch` in `sh -c` with
	// POSIX-safe quoting and the exit-6 → exit-1 mapping PG needs at
	// end-of-archive — see that package's docstring for the full
	// rationale.
	rcmd := walfetchcmd.Build(m.binPath, opts.Deployment, opts.RepoURL)

	rec := &restore.Recovery{
		Enable:         true,
		RestoreCommand: rcmd,
		// pause: stay in recovery, readable, never auto-promote.
		// promote would diverge from production; shutdown ends the
		// session before queries run. pause is the only sensible
		// timetravel default.
		Action:   "pause",
		Timeline: "latest",
	}
	if !atTime.IsZero() {
		rec.TargetTime = atTime
	}
	if atLSN != "" {
		rec.TargetLSN = atLSN
	}

	if _, err := restore.Restore(ctx, restore.Options{
		RepoURL:        opts.RepoURL,
		Deployment:     opts.Deployment,
		BackupID:       pickedID,
		TargetDir:      opts.TargetDir,
		Verifier:       opts.Verifier,
		KEKForRef:      opts.KEKForRef,
		UnwrapDEK:      opts.UnwrapDEK,
		AllowOverwrite: opts.AllowOverwrite,
		Recovery:       rec,
	}); err != nil {
		return nil, fmt.Errorf("timetravel: restore: %w", err)
	}

	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	s := Session{
		Name:       opts.Name,
		Deployment: opts.Deployment,
		RepoURL:    opts.RepoURL,
		BackupID:   pickedID,
		TargetDir:  opts.TargetDir,
		TargetSpec: opts.At,
		TargetTime: atTime,
		TargetLSN:  atLSN,
		PGVersion:  pgVersion,
		CreatedAt:  now,
		ExpiresAt:  now.Add(ttl),
	}
	state.Sessions = append(state.Sessions, s)
	if err := m.saveStateLocked(state); err != nil {
		return nil, fmt.Errorf("timetravel: persist state: %w", err)
	}
	return &s, nil
}

// pickBackupForTarget walks the deployment's manifests and returns
// the latest committed backup whose StoppedAt is at-or-before the
// target time. When target is zero (LSN-only mode), we pick the
// latest committed backup overall and let PG's own recovery decide
// when to stop.
func (m *Manager) pickBackupForTarget(ctx context.Context, repoURL, deployment string, target time.Time, verifier *backup.Verifier) (string, int, error) {
	rs, err := openManifestStore(ctx, repoURL)
	if err != nil {
		return "", 0, err
	}
	defer rs.Close()

	var bestID string
	var bestStopped time.Time
	var bestVersion int
	for mm, lerr := range rs.store.List(ctx, deployment, verifier) {
		if lerr != nil || mm == nil {
			continue
		}
		if !target.IsZero() && mm.StoppedAt.After(target) {
			continue
		}
		if mm.StoppedAt.After(bestStopped) {
			bestStopped = mm.StoppedAt
			bestID = mm.BackupID
			bestVersion = mm.PGVersion
		}
	}
	if bestID == "" {
		if target.IsZero() {
			return "", 0, fmt.Errorf("timetravel: no committed backups for deployment %q", deployment)
		}
		return "", 0, fmt.Errorf("timetravel: no committed backup of %q is at-or-before %s",
			deployment, target.Format(time.RFC3339))
	}
	return bestID, bestVersion, nil
}

// List returns every recorded session, sorted by name. Set
// includeExpired=true to include sessions past their TTL.
func (m *Manager) List(includeExpired bool) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.loadStateLocked()
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(state.Sessions))
	for _, s := range state.Sessions {
		if !includeExpired && s.IsExpired() {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DestroyOptions tunes Destroy.
type DestroyOptions struct {
	// RemoveTargetDir, when true, also rm -rf's the data dir.
	// Default false: same posture as standby — let the operator
	// inspect before wiping.
	RemoveTargetDir bool
}

// Destroy removes the named session from the state file.
func (m *Manager) Destroy(ctx context.Context, name string, opts DestroyOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, err := m.loadStateLocked()
	if err != nil {
		return err
	}
	idx := -1
	for i, s := range state.Sessions {
		if s.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	target := state.Sessions[idx].TargetDir
	state.Sessions = append(state.Sessions[:idx], state.Sessions[idx+1:]...)
	if err := m.saveStateLocked(state); err != nil {
		return err
	}
	if opts.RemoveTargetDir {
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("timetravel: remove target %s: %w", target, err)
		}
	}
	return nil
}

// CleanupResult summarises a Cleanup pass.
type CleanupResult struct {
	Reaped          []string `json:"reaped"`
	RemainingActive int      `json:"remaining_active"`
}

// Cleanup destroys every expired session. Optionally also removes
// each reaped session's data dir.
func (m *Manager) Cleanup(ctx context.Context, removeTargets bool) (*CleanupResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, err := m.loadStateLocked()
	if err != nil {
		return nil, err
	}
	res := &CleanupResult{}
	keep := state.Sessions[:0]
	for _, s := range state.Sessions {
		if !s.IsExpired() {
			keep = append(keep, s)
			continue
		}
		res.Reaped = append(res.Reaped, s.Name)
		if removeTargets {
			if err := os.RemoveAll(s.TargetDir); err != nil {
				return nil, fmt.Errorf("timetravel cleanup: remove %s: %w", s.TargetDir, err)
			}
		}
	}
	state.Sessions = keep
	res.RemainingActive = len(state.Sessions)
	if err := m.saveStateLocked(state); err != nil {
		return nil, err
	}
	return res, nil
}

// ErrAlreadyExists / ErrNotFound mirror the standby package.
var (
	ErrAlreadyExists = errors.New("timetravel: name already exists")
	ErrNotFound      = errors.New("timetravel: session not found")
)

// --- target resolution ------------------------------------------------

// resolveTarget interprets the --at value as either a wall-clock
// time or a PG LSN. Precedence:
//
//  1. LSN form ("M/N" with hex chars) → TargetLSN.
//  2. RFC3339 / natural-time → TargetTime.
//
// The naturaltime parser handles "5 minutes ago", "yesterday 9pm",
// and absolute timestamps; we delegate to it after the LSN check.
func resolveTarget(at string, now time.Time) (time.Time, string, error) {
	if isLSN(at) {
		return time.Time{}, at, nil
	}
	t, err := naturaltime.Parse(at, now)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("timetravel: --at %q: %w", at, err)
	}
	return t.UTC(), "", nil
}

// isLSN tests whether s looks like a PG LSN: "<hex>/<hex>".
func isLSN(s string) bool {
	i := strings.IndexByte(s, '/')
	if i <= 0 || i == len(s)-1 {
		return false
	}
	for _, b := range s {
		if b == '/' {
			continue
		}
		if (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F') {
			continue
		}
		return false
	}
	return true
}

// --- state file -------------------------------------------------------

func (m *Manager) loadStateLocked() (*stateFileBody, error) {
	body, err := os.ReadFile(m.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &stateFileBody{Schema: SchemaStateFile}, nil
		}
		return nil, fmt.Errorf("timetravel: read %s: %w", m.statePath, err)
	}
	var s stateFileBody
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("timetravel: parse %s: %w", m.statePath, err)
	}
	if s.Schema == "" {
		s.Schema = SchemaStateFile
	}
	if s.Schema != SchemaStateFile {
		return nil, fmt.Errorf("timetravel: state file schema %q is not supported; want %q", s.Schema, SchemaStateFile)
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
