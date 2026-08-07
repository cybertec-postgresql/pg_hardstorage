//go:build !mutation_prestream_gap_ignores_frontier

package cli

import (
	"context"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/inventory"
)

// preStreamGapStart returns where the uncovered window actually BEGINS
// for a fresh-slot stream start: at the archive frontier when one
// exists, else at the oldest live backup's stop.
//
// The distinction is the difference between the two scenarios that
// reach the fresh-slot path. On a first-ever stream (`init --quick`
// then `wal stream`) nothing is archived, and the window opens at the
// backup's stop — minStop is exact. After a PATRONI FAILOVER the
// reconnect also lands here (the slot did not survive onto the new
// leader), but months of WAL may be archived: the window opens where
// ARCHIVING stopped, not where the oldest backup did. Recording
// [minStop, start) in that case claims already-archived WAL as
// missing — and because gap records are eternal, every unbounded
// restore from every backup older than the failover would refuse
// forever, training operators to --skip-gap-check past the refusals
// that are true.
//
// The frontier lookup mirrors the coordinator's: current timeline
// first, then the nearest timeline BELOW (never max-across — WAL
// archived on an older timeline past the branch is diverged history
// and must not count as coverage). A probe failure falls back to
// minStop: over-recording refuses restores that would have worked —
// recoverable; under-recording lets one truncate silently — not.
func preStreamGapStart(ctx context.Context, sp storage.StoragePlugin, deployment string, timeline uint32, minStop pglogrepl.LSN) pglogrepl.LSN {
	frontier, found, err := inventory.HighestArchivedLSN(ctx, sp, deployment, timeline)
	if err != nil || !found {
		frontier, _, found, err = inventory.HighestArchivedLSNBefore(ctx, sp, deployment, timeline)
		if err != nil || !found {
			return minStop
		}
	}
	if frontier > minStop {
		return frontier
	}
	return minStop
}
