//go:build !mutation_restore_cmd_lenient_tail

package walfetchcmd

// oneShotTail is the exit-code discipline appended to the `wal fetch`
// invocation for ONE-SHOT recovery (PITR, --to-latest, time-travel,
// the verifier's sandbox cluster).
//
// PostgreSQL's restore_command contract (xlogarchive.c,
// RestoreArchivedFile) is narrower than it looks: EVERY plain nonzero
// exit — 1, 2, 8, 127, all of them — means "that file is not
// available", and during unbounded recovery "not available" means END
// OF ARCHIVE: stop replaying, promote, report success. Only a
// termination BY SIGNAL (other than SIGTERM, which is shutdown)
// aborts recovery. An earlier revision of this tail passed non-6
// codes through in the belief that they would "surface as a crash" —
// they never did: an S3 outage, an expired credential, a keyring
// refused for its file mode, a corrupted segment manifest, a chunk
// swept by gc, even the agent binary missing from the recovery
// environment (shell exit 127) — every one of them read as a clean
// end-of-archive, and the restore PROMOTED silently behind the data
// it was asked for.
//
// So the tail speaks the only language PG hears:
//
//	exit 0            → segment delivered
//	exit 6 (notfound) → exit 1: the genuine "no such segment",
//	                    PG's normal end-of-archive signal
//	anything else     → kill -s ABRT $$: the shell dies by signal,
//	                    recovery ABORTS loudly, the operator sees
//	                    the fault instead of a truncated success
//
// The trailing exit 125 is unreachable belt-and-braces should the
// self-kill ever fail.
//
// Own file so the mutation registry can carry the exact pre-fix
// pass-through variant.
func oneShotTail() string {
	return `ec=$?; [ $ec = 0 ] && exit 0; [ $ec = 6 ] && exit 1; kill -s ABRT $$; exit 125`
}
