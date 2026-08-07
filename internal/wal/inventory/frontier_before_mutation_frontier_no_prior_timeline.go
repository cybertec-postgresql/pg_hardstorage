//go:build mutation_frontier_no_prior_timeline

package inventory

import (
	"context"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// HighestArchivedLSNBefore — MUTATED variant: never finds a prior
// timeline, which is functionally the pre-c2c9aa4 world where nothing
// looked below the current timeline at all.
//
// Under this mutation a post-promotion resume falls through to the
// fresh-deployment branch and anchors at the new leader's position —
// silently skipping every byte since the old timeline's frontier — and
// the agent's coordinator reads the post-promotion frontier as a
// first-time bootstrap, suppressing the gap calculation on the one
// event it exists to measure.
//
// Caught by TestResolveStartLSN_AfterPromotion* (internal/cli) and
// TestArchiveFrontierForLeader_* (internal/cli).
func HighestArchivedLSNBefore(ctx context.Context, sp storage.StoragePlugin, deployment string, timeline uint32) (pglogrepl.LSN, uint32, bool, error) {
	return 0, 0, false, nil
}
