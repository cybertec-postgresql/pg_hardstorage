//go:build !mutation_target_reachable_off_by_one

// reachable_shape.go — targetReachable inclusive/exclusive boundary (real impl).
//
// Split into its own file so the mutation-testing harness can swap in
// a deliberately-broken variant under the
// mutation_target_reachable_off_by_one build tag. See
// internal/testkit/mutation/mutation.go.
package restore

import "github.com/jackc/pglogrepl"

// targetReachable reports whether a PITR target LSN can be reached by
// forward WAL replay from a backup whose consistency point is stop.
//
// PG replays WAL strictly forward from the backup's checkpoint, so the
// boundary differs by stop mode:
//
//   - Inclusive (PG default): recovery stops AT the target, so
//     target == stop is reachable → target >= stop.
//   - Exclusive (--to-exclusive): recovery stops JUST BEFORE the
//     target, so target == stop would stop one LSN earlier than the
//     checkpoint — unreachable. Strict greater-than is required →
//     target > stop.
//
// Without the exclusive strictness, `--to-lsn <stop> --to-exclusive`
// looked accepted but silently produced an end-of-WAL recovery.
//
// Mutation-testing note: a broken variant in
// reachable_shape_mutation_off_by_one.go (selected by
// `go test -tags=mutation_target_reachable_off_by_one`) drops the
// exclusive strictness and uses >= for both modes; the
// exclusive-equality test in plan_reachability_test.go must fail
// under the tag.
func targetReachable(target, stop pglogrepl.LSN, inclusive bool) bool {
	if inclusive {
		return target >= stop
	}
	return target > stop
}
