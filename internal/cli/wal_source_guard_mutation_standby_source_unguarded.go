//go:build mutation_standby_source_unguarded

package cli

import (
	"context"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// guardSourceIsPrimary — MUTATED variant: the exact pre-6fc79f8 world.
// Nothing asks pg_is_in_recovery, so after a failover a single-host
// --pg-connection reconnects to the DEMOTED node and archives
// second-hand WAL from a replica indefinitely — measured at 90 seconds
// of silent streaming from a demoted node before the guard existed.
// Caught by TestGuardSourceIsPrimary_UnreachableFailsOpenButWarns
// (which requires the probe-failed warning the real guard emits).
func guardSourceIsPrimary(ctx context.Context, d *output.Dispatcher, opts walStreamOptions) error {
	_ = ctx
	_ = d
	_ = opts
	return nil
}
