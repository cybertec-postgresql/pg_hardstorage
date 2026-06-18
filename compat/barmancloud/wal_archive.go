// wal_archive.go — barman-cloud-wal-archive shim: argv → native `wal push` (CNPG archive_command target).
package barmancloud

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// ExecuteWalArchive is the entry point for
// `barman-cloud-wal-archive [flags] <s3-path> <stanza> <wal-file>`.
//
// CNPG's archive_command pipeline calls this on every WAL
// rotation.  Argv shape (captured from the in-tree fixture):
//
//	barman-cloud-wal-archive --gzip
//	    --endpoint-url <url>
//	    --cloud-provider aws-s3
//	    s3://bucket/prefix
//	    <stanza-name>
//	    pg_wal/000000010000000000000006
//
// Native dispatch: `pg_hardstorage wal push <deployment>
// <segment-path> --repo <url>`.  We synthesise the segment
// path from PGDATA + the relative pg_wal/... arg the
// archive_command passes; the native CLI's wal-push reads
// the file's bytes off disk.  Auto-init runs if the repo
// doesn't exist yet.
//
// argv0 is os.Args[0] from the multi-call dispatcher; we
// pass it through to cobra for usage strings.
func ExecuteWalArchive(argv []string) int {
	var f commonFlags
	var stdout, stderr = os.Stdout, os.Stderr

	c := &cobra.Command{
		Use:           "barman-cloud-wal-archive [flags] <s3-path> <stanza> <wal-file>",
		Short:         "Archive a single WAL segment (CNPG archive_command target)",
		Args:          cobra.ExactArgs(3),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	c.SetOut(stdout)
	c.SetErr(stderr)
	attachCommonFlags(c, &f, false)
	c.RunE = func(cmd *cobra.Command, args []string) error {
		return runWalArchive(cmd, f, args[0], args[1], args[2])
	}
	c.SetArgs(argv)
	if err := c.Execute(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runWalArchive(cmd *cobra.Command, f commonFlags, s3Path, stanza, walRel string) error {
	env := readEnv()
	repoURL, err := buildRepoURL(s3Path, f, env)
	if err != nil {
		return err
	}
	deployment := env.deploymentName(stanza)

	// PG passes the WAL segment path via the standard
	// archive_command %p substitution which CNPG renders as
	// `pg_wal/000000...`.  That path is RELATIVE to PGDATA;
	// the native CLI's wal-push wants the absolute path so it
	// can mmap the segment.
	pgdata := envLookup("PGDATA")
	if pgdata == "" {
		return fmt.Errorf("pg-hardstorage-barmancloud: wal-archive: PGDATA env var unset")
	}
	absPath := pgdata + "/" + walRel

	args := []string{
		"wal", "push", deployment, absPath,
		"--repo", repoURL,
	}
	// barman-cloud-wal-archive's --gzip is per-segment
	// compression.  Native `wal push` derives compression from
	// the repo's level (set at `repo init --compression`), not
	// per-push.  We accept --gzip silently — the operator's
	// expectation (compressed WAL in the repo) is preserved by
	// the native default; explicit per-push compression knobs
	// land in a future native flag if/when needed.
	_ = f.gzip

	rc := dispatchWithAutoInit(cmd.ErrOrStderr(), repoURL, args)
	if rc != 0 {
		return fmt.Errorf("pg-hardstorage-barmancloud: wal-archive: native CLI exited %d", rc)
	}
	return nil
}
