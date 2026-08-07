//go:build mutation_restore_cmd_lenient_tail

package walfetchcmd

// oneShotTail — MUTATED variant: the exact pre-fix pass-through tail
// (bug #21). PostgreSQL treats every plain nonzero restore_command
// exit as "file not available" — end-of-archive during unbounded
// recovery — so passing infrastructure failures through as exit codes
// means an S3 outage, a decrypt refusal, a gc-swept chunk, or a
// missing agent binary all end recovery cleanly and PROMOTE silently
// behind. Only a signal-death aborts recovery; this tail never sends
// one.
func oneShotTail() string {
	return `ec=$?; [ $ec = 6 ] && exit 1 || exit $ec`
}
