// wal_restore.go — barman-cloud-wal-restore shim: argv → native `wal fetch` (CNPG restore_command target).
package barmancloud

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// ExecuteWalRestore is the entry point for
// `barman-cloud-wal-restore [flags] <s3-path> <stanza> <wal-name> <output-path>`.
//
// CNPG replicas use this as restore_command during streaming
// catchup (the captured argv is for streaming-replica WAL
// fetch when the primary's WAL is no longer on disk).  Argv
// shape:
//
//	barman-cloud-wal-restore
//	    --endpoint-url <url>
//	    --cloud-provider aws-s3
//	    s3://bucket/prefix
//	    <stanza-name>
//	    000000010000000000000004        # bare WAL name (NO pg_wal/ prefix)
//	    pg_wal/RECOVERYXLOG             # output path (relative to PGDATA)
//
// Native dispatch: `pg_hardstorage wal fetch <deployment>
// <wal-name> <output-abs-path> --repo <url>`.
//
// # Exit-code discipline
//
// This binary IS the restore_command, so it owns the contract
// internal/restore/walfetchcmd/tail.go spells out for the shell
// wrapper the native restore writes. PostgreSQL's restore_command
// contract (xlogarchive.c, RestoreArchivedFile) reads EVERY plain
// nonzero exit as "that segment is not available", and during
// unbounded recovery "not available" means END OF ARCHIVE: stop
// replaying, promote, report success. Only death BY SIGNAL aborts
// recovery.
//
// The shim used to collapse every native failure to exit 1. An S3
// outage, an expired credential, a keyring refused for its file mode,
// a chunk swept by gc — each one reached PostgreSQL as a clean
// end-of-archive, and the replica promoted with unreplayed WAL still
// in the repository. That is the exact silent-data-loss mode tail.go
// exists to prevent, reintroduced in the one path where this shim is
// the restore_command.
//
// So this entry point speaks the same three-way language:
//
//	native 0            → exit 0   (segment delivered)
//	native 6 (notfound) → exit 1   (the genuine "no such segment")
//	anything else       → exit 126 (recovery ABORTS loudly)
//
// 126 is not arbitrary. tail.go's shell wrapper aborts by killing
// itself with SIGABRT, which a Go binary cannot do cleanly (the
// runtime intercepts SIGABRT, prints a traceback and exits 2 — and 2
// is just another "not available"). PostgreSQL gives us a second
// door: RestoreArchivedFile classifies the result with
// wait_result_is_any_signal(rc, true), which returns true for a real
// signal AND for any exit status greater than 125. So exit 126 lands
// on the same FATAL branch a signal would, without dumping a Go
// traceback into the server log.
func ExecuteWalRestore(argv []string) int {
	var f commonFlags
	var stdout, stderr = os.Stdout, os.Stderr

	c := &cobra.Command{
		Use:           "barman-cloud-wal-restore [flags] <s3-path> <stanza> <wal-name> <output-path>",
		Short:         "Restore a single WAL segment (CNPG restore_command target)",
		Args:          cobra.ExactArgs(4),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	c.SetOut(stdout)
	c.SetErr(stderr)
	attachCommonFlags(c, &f, false)
	c.RunE = func(cmd *cobra.Command, args []string) error {
		return runWalRestore(cmd, f, args[0], args[1], args[2], args[3])
	}
	c.SetArgs(argv)
	if err := c.Execute(); err != nil {
		fmt.Fprintln(stderr, err)
		// A usage / argv / config failure never happened against the
		// repository at all, so it cannot be "no such segment".
		// Treat it the same as any other non-notfound fault: abort
		// recovery rather than let PG read it as end-of-archive.
		var fe *fetchError
		if errors.As(err, &fe) && fe.exitCode == int(output.ExitNotFound) {
			return 1
		}
		fmt.Fprintln(stderr,
			"pg-hardstorage-barmancloud: wal-restore: exiting "+
				strconv.Itoa(exitAbortRecovery)+" to ABORT recovery — this is a fetch FAILURE, "+
				"not an end of archive; PostgreSQL must not promote here")
		return exitAbortRecovery
	}
	return 0
}

// exitAbortRecovery is the status that makes PostgreSQL treat a failed
// restore_command as fatal rather than as the end of the archive.
// Anything greater than 125 satisfies wait_result_is_any_signal(rc,
// true) in PostgreSQL's RestoreArchivedFile.
const exitAbortRecovery = 126

// fetchError carries the native CLI's exit code so ExecuteWalRestore
// can tell "segment genuinely absent" (6) from every other fault.
type fetchError struct {
	exitCode int
	message  string
}

func (e *fetchError) Error() string { return e.message }

func runWalRestore(cmd *cobra.Command, f commonFlags, s3Path, stanza, walName, outRel string) error {
	env := readEnv()
	repoURL, err := buildRepoURL(s3Path, f, env)
	if err != nil {
		return err
	}
	deployment := env.deploymentName(stanza)

	// PG's restore_command rewrites %f → bare WAL name
	// (000000010000000000000004) and %p → relative output
	// path (pg_wal/RECOVERYXLOG).  We absolutise the output
	// against PGDATA so the native CLI can write the file
	// directly without a chdir.
	pgdata := envLookup("PGDATA")
	if pgdata == "" {
		return fmt.Errorf("pg-hardstorage-barmancloud: wal-restore: PGDATA env var unset")
	}
	absOut := pgdata + "/" + outRel

	args := []string{
		"wal", "fetch", deployment, walName, absOut,
		"--repo", repoURL,
	}

	// wal-restore is a read path — auto-init makes no sense
	// here.  If the repo doesn't exist, the native CLI's
	// notfound.repo is the right error to surface (the replica
	// has no WAL to fetch FROM, recovery cannot proceed).
	res := dispatchNative(args)
	forwardCaptured(res)
	if res.ExitCode != 0 {
		return &fetchError{
			exitCode: res.ExitCode,
			message: fmt.Sprintf("pg-hardstorage-barmancloud: wal-restore: native CLI exited %d",
				res.ExitCode),
		}
	}
	return nil
}
