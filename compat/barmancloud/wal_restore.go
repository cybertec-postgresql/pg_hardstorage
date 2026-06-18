// wal_restore.go — barman-cloud-wal-restore shim: argv → native `wal fetch` (CNPG restore_command target).
package barmancloud

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
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
		return 1
	}
	return 0
}

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
		return fmt.Errorf("pg-hardstorage-barmancloud: wal-restore: native CLI exited %d", res.ExitCode)
	}
	return nil
}
