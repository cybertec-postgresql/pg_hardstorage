// restore.go — barman-cloud-restore shim entry point: argv → native `pg_hardstorage restore` (CNPG bootstrap target).
package barmancloud

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// ExecuteRestore is the entry point for
// `barman-cloud-restore [flags] <s3-path> <stanza> <backup-id> <recovery-target-dir>`.
//
// CNPG fires this from its `Cluster.spec.bootstrap.recovery`
// reconciler when bootstrapping a new cluster from a backup.
// We don't have a captured fixture for this verb — CNPG only
// fires it during recovery bootstrap, which our discovery
// pass didn't trigger.  The argv shape is documented in the
// barman-cloud-restore Python source: positional args are
// (s3_path, stanza, backup_id, recovery_dir).
//
// Native dispatch: `pg_hardstorage restore <deployment>
// <backup-id> --target <recovery-dir> --repo <url>`.
func ExecuteRestore(argv []string) int {
	var f commonFlags
	var stdout, stderr = os.Stdout, os.Stderr

	c := &cobra.Command{
		Use:           "barman-cloud-restore [flags] <s3-path> <stanza> <backup-id> <recovery-dir>",
		Short:         "Restore a backup into a target directory (CNPG bootstrap target)",
		Args:          cobra.ExactArgs(4),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	c.SetOut(stdout)
	c.SetErr(stderr)
	attachCommonFlags(c, &f, false)
	c.RunE = func(cmd *cobra.Command, args []string) error {
		return runRestore(cmd, f, args[0], args[1], args[2], args[3])
	}
	c.SetArgs(argv)
	if err := c.Execute(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runRestore(cmd *cobra.Command, f commonFlags, s3Path, stanza, backupID, recoveryDir string) error {
	env := readEnv()
	repoURL, err := buildRepoURL(s3Path, f, env)
	if err != nil {
		return err
	}
	deployment := env.deploymentName(stanza)

	args := []string{
		"restore", deployment, backupID,
		"--target", recoveryDir,
		"--repo", repoURL,
	}

	res := dispatchNative(args)
	forwardCaptured(res)
	if res.ExitCode != 0 {
		return fmt.Errorf("pg-hardstorage-barmancloud: restore: native CLI exited %d", res.ExitCode)
	}
	return nil
}
