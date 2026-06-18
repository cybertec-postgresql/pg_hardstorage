// Package timeline persists PostgreSQL timeline-history files
// (TLI .history) into the repo so PITR can reconstruct the
// timeline lineage at restore time.
//
// On every promotion (Patroni failover, manual switchover,
// pg_ctl promote on a replica), PG increments the cluster
// timeline ID. The new timeline's .history file describes the
// switch points back to the cluster's first TLI. Restore needs
// every .history file along the chain to the target timeline,
// otherwise PG refuses to recover across the failover boundary
// with "could not find timeline N".
//
// The leader-follow loop captures these via TIMELINE_HISTORY
// over the replication protocol; this package writes them to
// the repo at:
//
//	wal/<deployment>/timelines/<tli>.history
//
// Idempotent: re-storing the same TLI is a no-op when the
// existing bytes match. A genuine bytes-mismatch (PG re-issued
// a different .history for the same TLI — unusual but possible
// after a forced rebuild) surfaces as ErrHistoryMismatch with
// both blobs visible to the operator.
package timeline

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	stdio "io"
	"strconv"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// tmpSuffix returns a random hex suffix so concurrent writers stage
// their tmp at DISTINCT keys. A shared (fixed) tmp path would let one
// agent's truncate-then-write leave the file partial at the instant
// another agent renames it — installing a truncated .history. Mirrors
// the randomized-tmp convention every other commit path uses (backup,
// walsink, repair).
func tmpSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is pathological; fall back to a
		// nanosecond stamp so the tmp key is still unlikely to collide.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// Path returns the canonical repo key for a deployment's TLI
// history file. Exposed so callers in the doctor / repair surface
// can probe + rewrite.
func Path(deployment string, tli uint32) string {
	return "wal/" + deployment + "/timelines/" + strconv.FormatUint(uint64(tli), 10) + ".history"
}

// Store wraps a StoragePlugin with timeline-history idempotency
// semantics. When WORM is non-nil, every committed .history file
// gets the configured retention deadline so a regulated repo
// retains the timeline lineage as long as it retains the
// committed backups it describes.
type Store struct {
	sp   storage.StoragePlugin
	worm *repo.WORMPolicy
}

// New wraps sp without WORM threading. Caller retains ownership.
func New(sp storage.StoragePlugin) *Store {
	if sp == nil {
		panic("timeline: nil StoragePlugin")
	}
	return &Store{sp: sp}
}

// NewWithRetention wraps sp + applies the WORM retention policy
// on every Put. When policy is nil/zero this is identical to
// New(sp). Same semantics as backup runner / WAL sink: each Put
// captures `now` at commit time so a re-run after a long pause
// produces a fresh retention deadline rather than a stale one.
func NewWithRetention(sp storage.StoragePlugin, policy *repo.WORMPolicy) *Store {
	if sp == nil {
		panic("timeline: nil StoragePlugin")
	}
	return &Store{sp: sp, worm: policy}
}

// Put writes content for the given (deployment, tli) pair.
//
// Semantics:
//
//   - First write at the path: writes content via
//     RenameIfNotExists semantics (see commit logic).
//   - Re-write with byte-identical content: no-op, returns nil.
//   - Re-write with different content: ErrHistoryMismatch with
//     the existing bytes attached for forensic inspection.
//
// The third case happens when PG issues a different .history
// for the same TLI (rare: usually after a forced rebuild via
// pg_resetwal that resurrects the timeline ID with new switch
// points). Operator-driven repair via `pg_hardstorage repair`
// is the right resolution; we refuse to silently overwrite the
// committed bytes from the live store.
func (s *Store) Put(ctx context.Context, deployment string, tli uint32, content []byte) error {
	if deployment == "" {
		return errors.New("timeline: empty deployment")
	}
	if tli == 0 {
		return errors.New("timeline: TLI 0 is invalid")
	}
	key := Path(deployment, tli)

	// Race-friendly: probe Stat first; on hit, compare bytes; on
	// match, return nil (idempotent re-put).
	if _, err := s.sp.Stat(ctx, key); err == nil {
		existing, gerr := s.read(ctx, key)
		if gerr != nil {
			return fmt.Errorf("timeline: read existing %s: %w", key, gerr)
		}
		if bytes.Equal(existing, content) {
			return nil
		}
		return &MismatchError{
			Deployment: deployment,
			Timeline:   tli,
			Existing:   existing,
			Incoming:   append([]byte(nil), content...),
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("timeline: stat %s: %w", key, err)
	}

	// First write. Use the standard tmp + RenameIfNotExists
	// pattern so concurrent leader-follow loops on two redundant
	// agents don't double-commit. The tmp suffix is RANDOM per writer:
	// a shared tmp path would let one agent's truncate-then-write leave
	// the file partial when another agent renames it, installing a
	// truncated .history.
	tmp := key + ".tmp." + tmpSuffix()
	putOpts := storage.PutOptions{ContentLength: int64(len(content))}
	if !s.worm.IsZero() {
		now := time.Now().UTC()
		putOpts.RetainUntil = s.worm.RetainUntil(now)
		putOpts.RetentionMode = storage.WORMMode(s.worm.Mode)
	}
	if _, err := s.sp.Put(ctx, tmp, bytes.NewReader(content), putOpts); err != nil {
		return fmt.Errorf("timeline: write tmp %s: %w", tmp, err)
	}
	if err := s.sp.RenameIfNotExists(ctx, tmp, key); err != nil {
		// Cleanup tmp; surface the rename error.
		_ = s.sp.Delete(ctx, tmp)
		// A racing winner committed the same bytes: re-check via
		// Stat → read; if it matches, treat as idempotent.
		if errors.Is(err, storage.ErrAlreadyExists) {
			existing, gerr := s.read(ctx, key)
			if gerr == nil && bytes.Equal(existing, content) {
				return nil
			}
			if gerr == nil {
				return &MismatchError{
					Deployment: deployment,
					Timeline:   tli,
					Existing:   existing,
					Incoming:   append([]byte(nil), content...),
				}
			}
		}
		return fmt.Errorf("timeline: install %s: %w", key, err)
	}
	return nil
}

// Get fetches the .history content for (deployment, tli). Returns
// storage.ErrNotFound when the file isn't yet committed.
func (s *Store) Get(ctx context.Context, deployment string, tli uint32) ([]byte, error) {
	if deployment == "" {
		return nil, errors.New("timeline: empty deployment")
	}
	return s.read(ctx, Path(deployment, tli))
}

func (s *Store) read(ctx context.Context, key string) ([]byte, error) {
	rc, err := s.sp.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return stdio.ReadAll(rc)
}

// MismatchError is returned by Put when the committed bytes for
// (deployment, tli) differ from the incoming bytes. Both blobs
// are surfaced so the operator can diff and decide whether to
// repair.
type MismatchError struct {
	Deployment string
	Timeline   uint32
	Existing   []byte
	Incoming   []byte
}

// Error implements error.
func (e *MismatchError) Error() string {
	return fmt.Sprintf("timeline: %s/timeline %d already committed with different bytes (existing=%d B, incoming=%d B); refusing to overwrite without explicit repair",
		e.Deployment, e.Timeline, len(e.Existing), len(e.Incoming))
}

// Is implements errors.Is so callers can match against the
// sentinel without needing the typed-error reference.
func (e *MismatchError) Is(target error) bool {
	return target == ErrHistoryMismatch
}

// ErrHistoryMismatch is the sentinel for errors.Is.
var ErrHistoryMismatch = errors.New("timeline: existing committed bytes differ from incoming")
