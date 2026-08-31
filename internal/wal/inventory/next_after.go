package inventory

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// NextArchivedLSNAtOrAfter returns the start LSN of the lowest archived
// segment on (deployment, timeline) whose start is >= from. It answers
// "where does the archive RESUME after this hole?" — the counterpart of
// FirstWALHoleInRange, which finds where coverage stops. found=false
// means no archived segment starts at or after from.
//
// Like the rest of this package it is segment-size-aware: names are
// ordered by the monotonic (log_id, seg_in_log) encoding and the true
// LSN is read from the winning segment's manifest, so the answer is
// exact for non-default segment sizes and across a >4 GiB log-id roll.
func NextArchivedLSNAtOrAfter(ctx context.Context, sp storage.StoragePlugin, deployment string, timeline uint32, from pglogrepl.LSN) (pglogrepl.LSN, bool, error) {
	if sp == nil || deployment == "" {
		return 0, false, fmt.Errorf("inventory: NextArchivedLSNAtOrAfter requires sp + deployment")
	}
	prefix := fmt.Sprintf("wal/%s/%08X/", deployment, timeline)
	var (
		best      pglogrepl.LSN
		bestFound bool
	)
	for info, lerr := range sp.List(ctx, prefix) {
		if lerr != nil {
			return 0, false, lerr
		}
		if cerr := ctx.Err(); cerr != nil {
			return 0, false, cerr
		}
		key := info.Key
		if !strings.HasSuffix(key, ".json") || segmentKeyIsStagingTemp(key) {
			continue
		}
		rc, gerr := sp.Get(ctx, key)
		if gerr != nil {
			continue // racing janitor; the segment is gone, not a candidate
		}
		raw, rerr := storage.ReadAllLimited(rc, storage.MaxMetadataBytes)
		_ = rc.Close()
		if rerr != nil {
			continue
		}
		m, perr := walsink.ParseSegmentManifest(raw)
		if perr != nil {
			continue
		}
		start, perr2 := pglogrepl.ParseLSN(m.StartLSN)
		if perr2 != nil {
			continue
		}
		if start >= from && (!bestFound || start < best) {
			best, bestFound = start, true
		}
	}
	return best, bestFound, nil
}
