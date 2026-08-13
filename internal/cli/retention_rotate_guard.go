//go:build !mutation_agent_rotation_abort_on_livedesc

package cli

import (
	"errors"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// retention_rotate_guard.go — the agent's scheduled rotation soft-deletes
// each backup the policy selected. Two refusals are expected, not
// run-fatal: a held manifest is retention-immune, AND a parent kept alive
// by a held (or concurrently-committed) child cannot be tombstoned this run
// without orphaning that live child. Skipping BOTH is what stops one
// protected backup from wedging every nightly rotation (and leaving every
// deletable backup ordered after it un-reclaimed). Own file so the mutation
// registry can carry the variant that drops the live-descendants arm — the
// pre-fix wedge.
func retentionRotateSkippable(err error) bool {
	return errors.Is(err, backup.ErrManifestHeld) ||
		errors.Is(err, backup.ErrChainHasLiveDescendants)
}
