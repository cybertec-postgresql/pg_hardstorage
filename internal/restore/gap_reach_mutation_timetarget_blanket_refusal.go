//go:build mutation_timetarget_blanket_refusal

package restore

// gapReachableBySeed — MUTATED variant: every recorded gap counts,
// the exact pre-fix world (bug #18). preflightTimeTargetGap then
// refuses any time/name-target restore of a deployment with ANY gap
// record — including fix #17's pre-stream gaps, which are eternal:
// once retention expires the init-era backup, `--to <time>` refuses
// forever over a window no surviving backup's replay can reach.
func gapReachableBySeed(backupStopLSN, gapEndLSN string) bool {
	return true
}
