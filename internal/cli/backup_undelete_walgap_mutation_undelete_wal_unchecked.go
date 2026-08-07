//go:build mutation_undelete_wal_unchecked

package cli

// recordResurrectedWALGap — MUTATED variant: resurrection never looks
// at the WAL, the exact pre-fix world. `backup undelete` verifies
// chunks and hands back a backup whose forward window `wal prune`
// deleted while it was tombstoned; no gap record exists, so
// --to-latest promotes silently behind and a standby freezes forever,
// with only a Warning-severity contiguity event at restore time.

import (
	"context"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

func recordResurrectedWALGap(ctx context.Context, d *output.Dispatcher, sp storage.StoragePlugin, deployment, backupID string, m *backup.Manifest) string {
	return ""
}
