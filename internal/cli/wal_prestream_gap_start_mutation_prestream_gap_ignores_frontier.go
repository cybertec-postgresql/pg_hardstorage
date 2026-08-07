//go:build mutation_prestream_gap_ignores_frontier

package cli

// preStreamGapStart — MUTATED variant: the archive frontier is never
// consulted, the exact pre-fix world (bug #20). After a Patroni
// failover destroys the slot, the fresh-slot reconnect records
// [oldest-backup.stop, new-anchor) — claiming months of successfully
// archived WAL as a gap — and every unbounded restore from every
// backup older than the failover refuses forever.

import (
	"context"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

func preStreamGapStart(ctx context.Context, sp storage.StoragePlugin, deployment string, timeline uint32, minStop pglogrepl.LSN) pglogrepl.LSN {
	return minStop
}
