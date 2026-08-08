//go:build mutation_wal_removed_retried

package cli

// isWalRemovedError — MUTATED variant: PostgreSQL's "requested WAL
// segment has already been removed" is never recognised, so the
// stream retries the unfixable forever — issue #45's endless-loop
// shape for exactly this error, with the operator's actionable
// re-anchor guidance never surfacing.

func isWalRemovedError(err error) bool {
	return false
}
