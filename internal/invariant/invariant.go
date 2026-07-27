// Package invariant is the fail-closed assertion facility for
// pg_hardstorage's internal invariants.
//
// # Why assertions in a backup tool
//
// pg_hardstorage's one job is to hand back exactly the bytes it was
// given. For that job the failure ordering is strict: crashing loudly
// is ALWAYS better than continuing on corrupted internal state,
// because a crash costs one backup run while silently-wrong state
// costs the restore — discovered at the worst possible time. Three
// corruption-hunt audits found the same shape repeatedly: a
// violated-but-unchecked internal assumption (segment offsets,
// sequence linkage, chain parentage) let bad state flow into
// committed artifacts. Assertions are the cheap way to turn that
// class of bug from "silent corruption" into "loud aborted run".
//
// # What belongs in an assertion — and what does not
//
// Assert is ONLY for conditions that are impossible unless the
// PROGRAM is wrong: broken arithmetic, violated pre/post-conditions
// between our own functions, state-machine fields that our own code
// must have kept consistent. A violated assertion means "this binary
// has a bug", so the only safe continuation is none.
//
// Everything ENVIRONMENTAL — storage errors, partial reads, hostile
// or legacy on-disk data, concurrent writers, operator input — must
// stay ordinary error handling. Asserting on the environment turns
// recoverable situations into crashes; see the runner's structured
// errors for that layer.
//
// # Behaviour
//
// A violated assertion panics with an "invariant violation:" prefix.
// Panics are deliberate, not a debug-build toggle: the CLI dies
// loudly mid-run (fail-closed — nothing gets committed past the
// violation, because commits happen after the code that asserts),
// and the agent's schedule engine already recovers task panics into
// task failures, so one impossible state aborts that task without
// killing the fleet. There is intentionally no "disabled in
// production" mode — an invariant that is too expensive to check in
// production is too expensive to be an invariant (use a test
// instead), and one that is cheap should hold everywhere.
package invariant

import "fmt"

// Assert panics when cond is false. The message should state the
// violated invariant, not the symptom: "chunk hash must equal the
// hash of its bytes", not "bad hash".
func Assert(cond bool, format string, args ...any) {
	if !cond {
		panic("invariant violation: " + fmt.Sprintf(format, args...))
	}
}
