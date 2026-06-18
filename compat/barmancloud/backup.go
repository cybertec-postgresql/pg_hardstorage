// backup.go — barman-cloud-backup shim entry point: argv → native `pg_hardstorage backup` (CNPG Backup CRD target).
package barmancloud

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// ExecuteBackup is the entry point for
// `barman-cloud-backup [flags] <s3-path> <stanza>`.
//
// CNPG fires this from its `Backup` CRD reconciler.  Argv
// shape (captured fixture):
//
//	barman-cloud-backup --user postgres
//	    --name backup-20260508101757
//	    --gzip
//	    --endpoint-url <url>
//	    --cloud-provider aws-s3
//	    s3://bucket/prefix
//	    <stanza-name>
//
// Native dispatch: `pg_hardstorage backup <deployment>
// --pg-connection 'postgres://<user>@/postgres' --repo <url>
// --label <name>`.  PG host comes from PGHOST in env (CNPG
// sets it to /controller/run, the in-pod socket dir).
func ExecuteBackup(argv []string) int {
	var f commonFlags
	var stdout, stderr = os.Stdout, os.Stderr

	c := &cobra.Command{
		Use:           "barman-cloud-backup [flags] <s3-path> <stanza>",
		Short:         "Take a full backup (CNPG Backup CRD target)",
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	c.SetOut(stdout)
	c.SetErr(stderr)
	attachCommonFlags(c, &f, true /* withUserName */)
	c.RunE = func(cmd *cobra.Command, args []string) error {
		return runBackup(cmd, f, args[0], args[1])
	}
	c.SetArgs(argv)
	if err := c.Execute(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runBackup(cmd *cobra.Command, f commonFlags, s3Path, stanza string) error {
	env := readEnv()
	repoURL, err := buildRepoURL(s3Path, f, env)
	if err != nil {
		return err
	}
	deployment := env.deploymentName(stanza)

	// Build the libpq DSN.  CNPG's PGHOST is a socket dir
	// (/controller/run), PGUSER comes from env, PGPORT default
	// 5432.  --user from argv beats env.
	user := f.user
	if user == "" {
		user = env.pgUser
	}
	if user == "" {
		user = "postgres"
	}
	pgHost := env.pgHost
	if pgHost == "" {
		pgHost = "/var/run/postgresql"
	}
	port := env.pgPort
	if port == "" {
		port = "5432"
	}
	// Socket-style DSN.  When PGHOST is a directory, libpq
	// uses Unix sockets; the URL form `postgres://user@/db`
	// with no host triggers that.  Pass the socket dir via
	// the `host` query parameter so we don't have to URL-
	// encode the path.
	dsn := fmt.Sprintf("postgres://%s@/postgres?host=%s&port=%s",
		user, pgHost, port)

	args := []string{
		"backup", deployment,
		"--pg-connection", dsn,
		"--repo", repoURL,
	}
	if f.name != "" {
		args = append(args, "--label", f.name)
	}
	// --gzip is silently honoured at the repo level (compression
	// is set at `repo init`, not per-backup).  See wal_archive.go
	// for the same rationale.
	_ = f.gzip

	rc := dispatchWithAutoInit(cmd.ErrOrStderr(), repoURL, args)
	if rc != 0 {
		return fmt.Errorf("pg-hardstorage-barmancloud: backup: native CLI exited %d", rc)
	}
	return nil
}
