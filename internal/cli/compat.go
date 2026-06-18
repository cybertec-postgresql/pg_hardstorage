// compat.go — 'compat' CLI verb parent (groups translate subcommand for legacy tool migrations).
package cli

import (
	"github.com/spf13/cobra"
)

// newCompatCmd is the parent for `pg_hardstorage compat ...`
// subcommands.  Today it only carries `translate`; future
// shims (Barman) add their own sibling translators here.
func newCompatCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "compat <translate>",
		Short: "Compatibility helpers for legacy backup tools",
		Long: `compat groups operator helpers used during a migration
from a legacy backup tool (pgBackRest, Barman) to
pg_hardstorage.

Subcommands:

  translate    Convert a legacy config file into pg_hardstorage.yaml.

The drop-in CLI shims that mimic the legacy tools' command
surface live in their own binaries (pg-hardstorage-pgbackrest,
pg-hardstorage-barman); this subcommand is only for the
config-translation half of the migration story.`,
	}
	c.AddCommand(newCompatTranslateCmd())
	return c
}
