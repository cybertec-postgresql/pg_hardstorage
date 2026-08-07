//go:build mutation_undelete_argv_order

package cli

import (
	"context"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// orderAncestorsFirst — MUTATED variant: argv order verbatim, the
// exact pre-3af06d4 world. `backup delete --cascade` returns
// cascade_deleted LEAF-first and the store refuses resurrecting an
// incremental under a tombstoned ancestor, so the documented unwind —
// "pass the slice straight back" — fails on the first ID, right when
// an operator is following instructions verbatim under stress. Caught
// by TestCascadeUnwind_RoundTripRestoresTheChain.
func orderAncestorsFirst(ctx context.Context, store *backup.ManifestStore, deployment string, ids []string, verifier *backup.Verifier) []string {
	_ = ctx
	_ = store
	_ = deployment
	_ = verifier
	return ids
}
