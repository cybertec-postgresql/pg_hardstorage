// listbackup.go — Barman shim verb: `barman list-backup <server>` → native `pg_hardstorage list` (incl. --minimal).
package barman

import (
	"io"

	"github.com/spf13/cobra"
)

// newListBackupCmd handles `barman list-backup <server>`.
//
// Native dispatch: `pg_hardstorage list <server>`.
//
// Output is whatever the native `list` renders.  Operators with
// grep-based monitoring on Barman's "Backup <id>" pattern need to
// adjust their patterns to the native renderer's output (or pass
// `--output json` for stable schemas) — semantic equivalence, not
// byte equivalence, per compat/README.md.
//
// We accept and silently honour `--minimal` (Barman shows only the
// IDs) by translating to `--output template` with a tiny template
// that emits one ID per line.  That keeps the most common
// "list-backup | head -1" cron-monitoring pattern working.
func newListBackupCmd(stdout, stderr io.Writer) *cobra.Command {
	var minimal bool
	c := &cobra.Command{
		Use:   "list-backup <server>",
		Short: "List backups for a server (Barman compat)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			server := args[0]
			native := []string{"list", server}
			if minimal {
				native = append(native,
					"--output", "template",
					"--template", `{{range .backups}}{{.id}}
{{end}}`,
				)
			}
			native, err := injectDeploymentFlags(native, server, false)
			if err != nil {
				return err
			}
			return dispatchNative(stdout, stderr, native)
		},
	}
	c.Flags().BoolVar(&minimal, "minimal", false, "Barman: show only backup IDs, one per line")
	c.SilenceUsage = true
	return c
}
