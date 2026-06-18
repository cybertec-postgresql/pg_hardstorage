// root.go — WAL-G shim root: top-level `wal-g` cobra tree wiring the five supported verbs (backup/wal push/fetch + list).
package walg

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// NewRoot returns the cobra command tree exposed by
// pg-hardstorage-walg.  cmd/pg-hardstorage-walg mounts this directly
// at top level so the binary presents itself as `wal-g`.
//
// Verbs implemented (5): backup-push, backup-fetch, backup-list,
// wal-push, wal-fetch.  Anything else lands in refuseUnimplemented
// with a clear remediation pointing at the native equivalent.
func NewRoot(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:   "wal-g",
		Short: "pg_hardstorage drop-in shim for WAL-G",
		Long: `pg-hardstorage-walg mimics the WAL-G command surface so
existing cron jobs, archive_command lines, and monitoring scripts
keep working after the binary is symlinked into PATH as ` + "`wal-g`" + `.

Backups, WAL archive, and restores produced by this shim are
native pg_hardstorage artefacts — chunked through FastCDC, signed,
optionally KMS-wrapped.  Old WAL-G repos remain readable only by
WAL-G; the shim does not parse them.

Configuration follows the WAL-G env-var convention (WALG_S3_PREFIX,
PGHOST, ...).  See docs/how-to/migration/from-walg.md for the
cutover playbook.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Suppress cobra's default help / completion children — WAL-G
	// doesn't have them and we don't want shim users to see surface
	// area that isn't real.
	root.SetHelpCommand(&cobra.Command{Hidden: true})
	root.CompletionOptions.DisableDefaultCmd = true

	root.AddCommand(
		newBackupPushCmd(stderr),
		newBackupFetchCmd(stderr),
		newBackupListCmd(stderr),
		newWalPushCmd(stderr),
		newWalFetchCmd(stderr),
	)

	// Refusal-only verbs.  These are the most-cited WAL-G verbs
	// outside the v1.1 surface; each gets a tiny child that prints
	// the canonical refusal and exits 2.  Surfacing remediation at
	// the verb itself is friendlier than Cobra's "unknown command"
	// generic error.
	for _, r := range refusedVerbs {
		root.AddCommand(newRefusalCmd(stderr, r.name, r.suggestion))
	}

	// Anything still unmatched falls through here.
	root.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		return refuseUnimplemented(stderr, args[0],
			"see `pg_hardstorage --help` for the native command surface")
	}
	root.Args = cobra.ArbitraryArgs

	return root
}

// Execute parses os.Args and runs the shim.
// cmd/pg-hardstorage-walg/main.go calls this directly.
//
// Refusals (unknown verb, libsodium key set, etc.) exit 2 — matching
// the pgBackRest and Barman shims.  Other failures exit 1.
func Execute() int {
	root := NewRoot(os.Stdout, os.Stderr)
	if err := root.Execute(); err != nil {
		// Refusals already printed to stderr; print other errors.
		if _, ok := err.(*shimError); !ok {
			fmt.Fprintln(os.Stderr, err)
		}
		return ExitCode(err)
	}
	return 0
}

// refusedVerb is one entry in the static refusal table.
type refusedVerb struct {
	name       string
	suggestion string
}

var refusedVerbs = []refusedVerb{
	{"delete", "use `pg_hardstorage rotate <deployment> --apply` for retention pruning"},
	{"backup-mark", "use `pg_hardstorage hold add <deployment> <backup-id>` for legal hold / permanent retention"},
	{"catchup-push", "no equivalent — native uses physical replication; see `pg_hardstorage standby create`"},
	{"catchup-fetch", "no equivalent — native uses physical replication; see `pg_hardstorage standby create`"},
	{"wal-receive", "use `pg_hardstorage agent` (continuous, slot-based WAL streaming)"},
	{"wal-verify", "use `pg_hardstorage verify <deployment> <backup-id>`"},
	{"wal-show", "use `pg_hardstorage list <deployment>` and `wal list`"},
	{"st", "no equivalent — native repository is self-describing"},
	{"copy", "use `pg_hardstorage repo replicate` for cross-region copy"},
	{"daemon", "use `pg_hardstorage agent` (systemd-managed)"},
	{"backup-show", "use `pg_hardstorage show <deployment> <backup-id>`"},
}

// newRefusalCmd registers a verb that always refuses with a clear
// remediation.  Used for WAL-G commands that deliberately have no
// pg_hardstorage equivalent (different architecture) or that the
// v1.1 surface has not yet covered.
func newRefusalCmd(stderr io.Writer, name, suggestion string) *cobra.Command {
	c := &cobra.Command{
		Use:   name,
		Short: "Not implemented in v1.1 (WAL-G compat refusal)",
		// Accept any args so the user gets the remediation
		// regardless of what they typed after the verb.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return refuseUnimplemented(stderr, name, suggestion)
		},
	}
	c.SilenceUsage = true
	c.FParseErrWhitelist.UnknownFlags = true
	return c
}
