//go:build !mutation_timetarget_blanket_refusal

package restore

import "github.com/jackc/pglogrepl"

// gapReachableBySeed reports whether a recorded WAL gap can be reached
// by a restore seeded from a backup whose stop LSN is backupStopLSN.
// Replay starts at the seed's stop, so a gap ending at or below it is
// history the restore never touches. Unknown inputs — an empty or
// unparsable stop (older manifests predate StopLSN), an unparsable gap
// end — count as reachable: refusing wrongly is recoverable, allowing
// wrongly is not.
//
// Both the time/name-target refusal and its advisory warning filter
// through this ONE predicate. Its precision is what keeps fix #17's
// eternal gap records (gapstate is per-deployment; a pre-stream gap
// outlives the backups it described) from permanently refusing every
// `--to <time>` restore once retention expires the pre-gap generation.
// Own file so the mutation registry can carry the exact pre-fix
// blanket variant.
func gapReachableBySeed(backupStopLSN, gapEndLSN string) bool {
	stop, serr := pglogrepl.ParseLSN(backupStopLSN)
	if serr != nil || stop == 0 {
		return true // no stop to bound by: keep the blanket posture
	}
	ge, gerr := pglogrepl.ParseLSN(gapEndLSN)
	return gerr != nil || ge > stop
}
