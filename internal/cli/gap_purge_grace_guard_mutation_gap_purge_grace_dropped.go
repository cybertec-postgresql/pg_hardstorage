//go:build mutation_gap_purge_grace_dropped

package cli

import (
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// tombstonedTimelineStillLive — MUTATED: every tombstoned timeline is
// treated as dead, ignoring the GC grace — the pre-fix world where
// `gap-purge --orphans` reaps a gap that a within-grace, still-undeleteable
// backup's recovery_target_timeline='latest' PITR crosses, silently losing
// the refusal that would have flagged the missing WAL.
func tombstonedTimelineStillLive(_ error, _ *backup.Tombstone, _ time.Time) bool {
	return false
}
