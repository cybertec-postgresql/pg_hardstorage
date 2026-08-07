//go:build mutation_history_preflight_absent

package restore

// preflightTimelineHistory — MUTATED variant: the check does not
// exist, the exact pre-fix world (bug #22). PostgreSQL probes
// <N>.history ascending and stops at the first miss, so one
// unarchived or lost history file makes a --to-latest restore
// silently end recovery on an older timeline and promote — success
// reported, every newer-timeline segment in the archive ignored.

import (
	"context"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

func preflightTimelineHistory(ctx context.Context, sp storage.StoragePlugin, deployment string, seedTLI uint32, recovery *Recovery, emit func(*output.Event)) error {
	return nil
}
