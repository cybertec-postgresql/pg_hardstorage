//go:build mutation_target_reachable_off_by_one

package restore

import "github.com/jackc/pglogrepl"

// targetReachable — MUTATED variant. Drops the exclusive-stop
// strictness and uses >= for BOTH modes, re-introducing the bug where
// `--to-lsn <stop> --to-exclusive` is accepted even though recovery
// would stop one LSN before the backup's checkpoint and silently run
// to end-of-WAL instead.
//
// Selected by `go test -tags=mutation_target_reachable_off_by_one`.
// TestCheckTargetReachable_LSN_EqualsStop_Exclusive_Refuses in
// plan_reachability_test.go must fail under this mutation.
//
// See internal/testkit/mutation/mutation.go.
func targetReachable(target, stop pglogrepl.LSN, _ bool) bool {
	// NOTE: the `inclusive` flag is intentionally ignored — the
	// exclusive branch (strict >) is missing, so an exclusive target
	// exactly at stop wrongly reports reachable.
	return target >= stop
}
