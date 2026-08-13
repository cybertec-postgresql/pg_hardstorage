//go:build mutation_agent_rotation_abort_on_livedesc

package cli

import (
	"errors"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// retentionRotateSkippable — MUTATED: only a held manifest is skipped, so a
// parent kept alive by its held child returns ErrChainHasLiveDescendants
// which the agent treats as run-fatal — the pre-fix world where one hold
// aborts every scheduled rotation and any backup ordered after the held
// anchor is never reclaimed (unbounded repo growth on the unattended path).
func retentionRotateSkippable(err error) bool {
	return errors.Is(err, backup.ErrManifestHeld)
}
