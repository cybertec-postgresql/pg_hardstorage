// check.go — pgBackRest shim verb: `pgbackrest check` → native `pg_hardstorage doctor <stanza>`.
package pgbackrest

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newCheckCmd implements `pgbackrest --stanza=<n> check`.
//
// Native dispatch: `pg_hardstorage doctor <stanza>`.
// The native doctor surfaces a richer set of checks (PG
// reachability, slot status, repo write probe, KEK/keyring
// presence, ...).  Operators looking for the lighter
// pgBackRest "is archive_command working?" probe should
// continue to use that semantically — the doctor's output
// is a strict superset.
func newCheckCmd() *cobra.Command {
	c := &cobra.Command{
		Use:           "check",
		Short:         "Health check the stanza",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCheck(globalArgs)
		},
	}
	return c
}

func runCheck(a pgbackrestArgs) error {
	if a.stanza == "" {
		return fmt.Errorf("pg-hardstorage-pgbackrest: check: --stanza is required")
	}
	// doctor doesn't need --pg-connection / --repo if the
	// stanza is configured in pg_hardstorage.yaml.  The
	// shim doesn't write that file (the translator
	// subcommand does); if the operator hasn't run the
	// translator, doctor will surface the missing config
	// itself.  We pass the stanza positional only.
	out := []string{"doctor", a.stanza}
	if rc := dispatchNative(out); rc != 0 {
		return fmt.Errorf("pg-hardstorage-pgbackrest: check: native CLI exited %d", rc)
	}
	return nil
}
