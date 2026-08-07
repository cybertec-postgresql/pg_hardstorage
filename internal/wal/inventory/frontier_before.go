//go:build !mutation_frontier_no_prior_timeline

package inventory

import (
	"context"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// HighestArchivedLSNBefore returns the archive frontier on the newest
// timeline strictly below `timeline`, along with the timeline it was
// found on.
//
// This answers the question a stream resume has to ask after a
// promotion. The new primary reports timeline N+1 via IDENTIFY_SYSTEM,
// but nothing has been archived under N+1 yet, so a lookup scoped to
// the current timeline returns found=false — which is indistinguishable
// from a genuinely fresh deployment unless somebody looks one timeline
// down. Those two cases need opposite handling: a fresh deployment may
// start wherever the slot allows, while a promoted one must not start
// past what the previous timeline already reached, or the WAL in
// between is never fetched.
//
// Scanning DOWNWARD and stopping at the first timeline with segments is
// deliberate, not an optimisation. WAL archived on an older timeline
// past the point where the newer one branched is DIVERGED history — the
// old primary kept writing before it was fenced — and replaying it
// would corrupt the lineage. The frontier that matters is the one on
// the timeline we branched from, which is the highest one below the
// current. Taking a max across all timelines would find the diverged
// bytes instead.
//
// In the common case — a single promotion from N to N+1 — the first
// probe hits and this costs one List.
func HighestArchivedLSNBefore(ctx context.Context, sp storage.StoragePlugin, deployment string, timeline uint32) (pglogrepl.LSN, uint32, bool, error) {
	if timeline <= 1 {
		// Timeline 1 is the bottom; there is nothing below it, so a
		// miss really is a fresh deployment.
		return 0, 0, false, nil
	}
	for tli := timeline - 1; tli >= 1; tli-- {
		if cerr := ctx.Err(); cerr != nil {
			return 0, 0, false, cerr
		}
		lsn, found, err := HighestArchivedLSN(ctx, sp, deployment, tli)
		if err != nil {
			return 0, 0, false, err
		}
		if found {
			return lsn, tli, true, nil
		}
	}
	return 0, 0, false, nil
}
