// wal_inventory.go — wires real wal/inventory.HighestArchivedLSN into the readiness package's stub.
package recovery

import (
	"context"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/inventory"
)

// init wires the real wal/inventory.HighestArchivedLSN call into
// the readiness package's stub. The package-level stub indirection
// lets test code mock the WAL inventory without dragging the
// pglogrepl dependency into every test file.
func init() {
	walHighestForTimelineImpl = func(ctx context.Context, sp storage.StoragePlugin, deployment string, timeline uint32) (string, bool, error) {
		lsn, found, err := inventory.HighestArchivedLSN(ctx, sp, deployment, timeline)
		if err != nil || !found {
			return "", false, err
		}
		return lsn.String(), true, nil
	}
}
