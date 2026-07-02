// flags.go — Barman shim flag table: `barman recover` arg struct and Barman→native flag translation.
package barman

import (
	"fmt"
	"io"
	"strings"
)

// recoverArgs is what `barman recover` parsing collects before the
// per-flag mapper turns it into native args.  Only the flags we
// actually translate (or explicitly drop) get a struct field; the
// "raw" slice carries everything else for refusal-with-context.
type recoverArgs struct {
	server      string
	backupID    string
	targetDir   string
	targetTime  string
	targetXID   string
	targetName  string
	targetImmed bool
	// pause | promote | shutdown
	targetAction string
	remoteSSHCmd string
	// nil = unset, true / false = explicit --get-wal / --no-get-wal
	getWAL   *bool
	retryN   string
	retrySlp string
	repoURL  string
	pgConn   string
	// Anything we did not recognise.
	unknown []string
}

// flagMap is the most-cited Barman -> native translation table.
// `apply` runs once per recover invocation: it appends the relevant
// native flags to `out`, prints a stderr warning for dropped flags,
// or returns a refusal if the Barman flag has no safe equivalent.
//
// Table-driven so adding a new mapping is one row.  Only the recover
// verb has enough flag breadth to justify the table; the simpler
// verbs map flags inline.
type flagMap struct {
	// pick reads from r and decides whether the row applies.
	pick func(r *recoverArgs) (active bool)
	// apply mutates `native` (appending flags), prints to warn (for
	// dropped flags), and may return a refusal error.
	apply func(r *recoverArgs, native *[]string, warn io.Writer) error
}

// mapToNativeArgs is the recover-flag translator.  Returns the
// slice that will be passed to root.SetArgs after the verb header.
//
// It does NOT prepend the verb itself — callers do that, because
// list-backup / show-backup / etc. each need a different verb header.
func mapToNativeArgs(r *recoverArgs, warn io.Writer) ([]string, error) {
	var native []string
	for _, row := range recoverFlagTable {
		if !row.pick(r) {
			continue
		}
		if err := row.apply(r, &native, warn); err != nil {
			return nil, err
		}
	}
	return native, nil
}

// recoverFlagTable is the canonical mapping for `barman recover`
// flags.  Order matters only for refusals (we want the most useful
// remediation to surface first), not correctness.
var recoverFlagTable = []flagMap{
	{
		// --target-time -> --to "<t>"
		pick: func(r *recoverArgs) bool { return r.targetTime != "" },
		apply: func(r *recoverArgs, n *[]string, _ io.Writer) error {
			*n = append(*n, "--to", r.targetTime)
			return nil
		},
	},
	{
		// --target-xid: not supported, refuse with remediation.
		pick: func(r *recoverArgs) bool { return r.targetXID != "" },
		apply: func(r *recoverArgs, _ *[]string, _ io.Writer) error {
			return fmt.Errorf(
				"--target-xid not supported by pg_hardstorage; " +
					"convert the XID to an LSN (SELECT pg_current_wal_lsn() near commit) " +
					"and use --to-lsn",
			)
		},
	},
	{
		// --target-name -> --to-name (recover to a PG named restore
		// point). Native `restore` has no --to-backup flag.
		pick: func(r *recoverArgs) bool { return r.targetName != "" },
		apply: func(r *recoverArgs, n *[]string, _ io.Writer) error {
			*n = append(*n, "--to-name", r.targetName)
			return nil
		},
	},
	{
		// --target-immediate -> NO target. A plain native restore (no
		// --to* flag) already stops at the backup's consistency point
		// (recovery_target='immediate'), which is exactly what Barman's
		// --target-immediate means. Mapping it to `--to-lsn 0/0` was a
		// bug: 0/0 is below any real stop LSN, so recovery could never
		// reach it and the restore was always refused.
		pick: func(r *recoverArgs) bool { return r.targetImmed },
		apply: func(_ *recoverArgs, _ *[]string, _ io.Writer) error {
			return nil
		},
	},
	{
		// --target-action -> --to-action (pause|promote|shutdown)
		pick: func(r *recoverArgs) bool { return r.targetAction != "" },
		apply: func(r *recoverArgs, n *[]string, warn io.Writer) error {
			a := strings.ToLower(r.targetAction)
			switch a {
			case "pause", "promote", "shutdown":
				*n = append(*n, "--to-action", a)
				return nil
			default:
				warnDroppedFlag(warn,
					"--target-action="+r.targetAction,
					"unknown action; native default (pause) used")
				return nil
			}
		},
	},
	{
		// --remote-ssh-command: drop + warn.
		pick: func(r *recoverArgs) bool { return r.remoteSSHCmd != "" },
		apply: func(_ *recoverArgs, _ *[]string, warn io.Writer) error {
			warnDroppedFlag(warn, "--remote-ssh-command",
				"native uses replication-protocol over libpq, no SSH needed")
			return nil
		},
	},
	{
		// --get-wal / --no-get-wal: drop + warn.
		pick: func(r *recoverArgs) bool { return r.getWAL != nil },
		apply: func(r *recoverArgs, _ *[]string, warn io.Writer) error {
			tag := "--get-wal"
			if !*r.getWAL {
				tag = "--no-get-wal"
			}
			warnDroppedFlag(warn, tag,
				"native always fetches WAL from the configured repo")
			return nil
		},
	},
	{
		// --retry-times / --retry-sleep: drop + warn.
		pick: func(r *recoverArgs) bool { return r.retryN != "" || r.retrySlp != "" },
		apply: func(_ *recoverArgs, _ *[]string, warn io.Writer) error {
			warnDroppedFlag(warn, "--retry-times/--retry-sleep",
				"native has built-in exponential backoff")
			return nil
		},
	},
}
