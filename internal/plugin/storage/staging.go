package storage

// staging.go — reaping backend staging temps a crash left behind.
//
// The fs, sftp and scp backends write every object to a staging temp
// first (".hstmp-", ".deferred-", ".excl-") and publish it by rename
// or link. Those names are hidden from List on purpose — they are not
// objects — which also hides them from every GC pass. A process that
// dies between the write and the publish (crash, OOM kill, a
// connection lost mid-Put) leaves the temp behind, and nothing ever
// removed it: the disk leaked for good.

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultStagingReapAge is how old a staging temp must be before
// `repo gc` reaps it. A temp lives for one Put — or, for an fs
// DurabilityDeferred chunk, until the next Barrier — so a day is far
// beyond any live writer.
const DefaultStagingReapAge = 24 * time.Hour

// MinStagingReapAge is the floor ReapStagingOf enforces. A younger temp
// may belong to a Put still in flight in another process; deleting it
// would fail that Put's publish (or, for a deferred chunk, its
// Barrier).
const MinStagingReapAge = time.Hour

// ErrStagingAgeTooShort is returned by ReapStagingOf for an olderThan
// below MinStagingReapAge.
var ErrStagingAgeTooShort = errors.New("storage: staging reap age below the safety floor")

// ReapStats reports what a staging reap removed.
type ReapStats struct {
	// Removed is the number of staging temps deleted.
	Removed int `json:"removed"`
	// Bytes is their total size, where the backend reports it.
	Bytes int64 `json:"bytes"`
	// Unsupported is true when the backend has no staging temps to
	// reap (object stores publish atomically without them).
	Unsupported bool `json:"unsupported,omitempty"`
}

// StagingReapAware is an OPTIONAL interface: a backend that stages
// writes under hidden temp names removes the ones last modified more
// than olderThan ago. Only backend-internal staging names are touched,
// never a caller's key (the repo layer's "<name>.tmp.<rand>" manifest
// temps are visible keys with their own reaper).
type StagingReapAware interface {
	ReapStaging(ctx context.Context, olderThan time.Duration) (ReapStats, error)
}

// ReapStagingOf reaps sp's stale staging temps when sp implements
// StagingReapAware, and reports Unsupported otherwise. olderThan below
// MinStagingReapAge is refused.
func ReapStagingOf(ctx context.Context, sp StoragePlugin, olderThan time.Duration) (ReapStats, error) {
	if olderThan < MinStagingReapAge {
		return ReapStats{}, fmt.Errorf("%w: %s < %s", ErrStagingAgeTooShort, olderThan, MinStagingReapAge)
	}
	if r, ok := sp.(StagingReapAware); ok {
		return r.ReapStaging(ctx, olderThan)
	}
	return ReapStats{Unsupported: true}, nil
}
