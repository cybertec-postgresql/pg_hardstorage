//go:build !mutation_gap_purge_grace_dropped

package cli

import (
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// gap_purge_grace_guard.go — `wal gap-purge --orphans` reaps gap records
// whose timeline is below every restorable backup. A soft-deleted backup
// is still restorable via `backup undelete` until its tombstone ages past
// the GC grace, so within-grace tombstoned timelines MUST still count — or
// --orphans reaps a gap an undeleted backup's PITR would cross and restore
// silently loses the refusal. gc / wal-prune already honour this grace;
// this must match them. Own file so the mutation registry can carry the
// variant that excludes ALL tombstoned timelines (the pre-fix hole).
func tombstonedTimelineStillLive(readErr error, ts *backup.Tombstone, graceCutoff time.Time) bool {
	if readErr != nil || ts == nil || ts.TombstonedAt.IsZero() {
		// Age unknowable → keep (over-keeping a tiny gap record is safe;
		// reaping a still-needed one is not).
		return true
	}
	return !ts.TombstonedAt.Before(graceCutoff)
}
