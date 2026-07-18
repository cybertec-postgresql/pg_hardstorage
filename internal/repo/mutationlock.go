package repo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// MutationLockKey serializes manifest publication with destructive GC. Chunk
// uploads may proceed without the lock, but every manifest publisher must hold
// it and re-check its chunks before publication. GC holds it from reference
// collection through deletion, so it cannot delete from a stale snapshot and
// then race a manifest into existence.
const MutationLockKey = "_locks/repository-mutation.json"

var ErrMutationLocked = errors.New("repo: another repository mutation is in progress")

// MutationLockedError describes the current holder when its lock record can
// be read. Callers may use Purpose to distinguish a duplicate operation they
// can briefly wait for from an unrelated mutation that should fail fast.
type MutationLockedError struct {
	Purpose string
}

func (e *MutationLockedError) Error() string {
	if e.Purpose == "" {
		return fmt.Sprintf("%v (lock key %s)", ErrMutationLocked, MutationLockKey)
	}
	return fmt.Sprintf("%v: %s (lock key %s)", ErrMutationLocked, e.Purpose, MutationLockKey)
}

func (e *MutationLockedError) Unwrap() error { return ErrMutationLocked }

type mutationLockBody struct {
	Owner     string    `json:"owner"`
	Purpose   string    `json:"purpose"`
	CreatedAt time.Time `json:"created_at"`
}

type MutationLock struct {
	sp    storage.StoragePlugin
	owner string
}

// AcquireMutationLock atomically acquires the repository mutation lock. Locks
// deliberately do not expire automatically: breaking a lock that still has a
// live holder re-opens the GC/commit race. A lock left by a crashed process is
// fail-closed and can be removed only after an operator confirms no writer or
// GC process is active.
func AcquireMutationLock(ctx context.Context, sp storage.StoragePlugin, purpose string) (*MutationLock, error) {
	if sp == nil {
		return nil, errors.New("repo: mutation lock requires storage")
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("repo: mutation lock owner: %w", err)
	}
	owner := hex.EncodeToString(raw[:])
	body, err := json.Marshal(mutationLockBody{Owner: owner, Purpose: purpose, CreatedAt: time.Now().UTC()})
	if err != nil {
		return nil, err
	}
	for {
		_, err = sp.Put(ctx, MutationLockKey, bytes.NewReader(body), storage.PutOptions{
			IfNotExists: true, ContentLength: int64(len(body)),
		})
		if errors.Is(err, storage.ErrAlreadyExists) {
			held, readErr := readMutationLockBody(ctx, sp)
			if errors.Is(readErr, storage.ErrNotFound) {
				// The holder released between our conditional Put and Get.
				// Retry the acquisition instead of surfacing a false conflict.
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}
				continue
			}
			if readErr != nil {
				return nil, fmt.Errorf("%w (lock key %s; holder metadata unreadable: %v)", ErrMutationLocked, MutationLockKey, readErr)
			}
			return nil, &MutationLockedError{Purpose: held.Purpose}
		}
		if err != nil {
			return nil, fmt.Errorf("repo: acquire mutation lock: %w", err)
		}
		return &MutationLock{sp: sp, owner: owner}, nil
	}
}

// Release removes the lock only when the stored owner still matches.
func (l *MutationLock) Release(ctx context.Context) error {
	if l == nil || l.sp == nil {
		return nil
	}
	held, err := readMutationLockBody(ctx, l.sp)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("repo: read mutation lock for release: %w", err)
	}
	if held.Owner != l.owner {
		return errors.New("repo: mutation lock ownership changed; refusing to release another holder's lock")
	}
	if err := l.sp.Delete(ctx, MutationLockKey); err != nil {
		return fmt.Errorf("repo: release mutation lock: %w", err)
	}
	l.sp = nil
	return nil
}

func readMutationLockBody(ctx context.Context, sp storage.StoragePlugin) (mutationLockBody, error) {
	rc, err := sp.Get(ctx, MutationLockKey)
	if err != nil {
		return mutationLockBody{}, err
	}
	body, readErr := io.ReadAll(io.LimitReader(rc, 1<<20))
	closeErr := rc.Close()
	if readErr != nil {
		return mutationLockBody{}, fmt.Errorf("repo: read mutation lock body: %w", readErr)
	}
	if closeErr != nil {
		return mutationLockBody{}, fmt.Errorf("repo: close mutation lock body: %w", closeErr)
	}
	var held mutationLockBody
	if err := json.Unmarshal(body, &held); err != nil {
		return mutationLockBody{}, fmt.Errorf("repo: decode mutation lock: %w", err)
	}
	if held.Owner == "" {
		return mutationLockBody{}, errors.New("repo: mutation lock has no owner")
	}
	return held, nil
}
