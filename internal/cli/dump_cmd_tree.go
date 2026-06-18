// dump_cmd_tree.go — hidden '__dump-cmd-tree' verb: emits the cobra tree as JSON for coverage gates.
package cli

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// newDumpCmdTreeCmd emits the entire cobra tree as a flat JSON list,
// one entry per node: { "path": "wal stream", "runnable": true,
// "has_subcommands": false, "hidden": false }.
//
// Hidden from --help (Hidden=true).  Used by the testkit's
// `coverage cli` gate to enumerate every leaf the CLI exposes
// without parsing --help output.
func newDumpCmdTreeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__dump-cmd-tree",
		Short:  "dump the cobra tree as JSON (internal; coverage gate)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			type node struct {
				Path           string `json:"path"`
				Runnable       bool   `json:"runnable"`
				HasSubcommands bool   `json:"has_subcommands"`
				Hidden         bool   `json:"hidden"`
			}
			var nodes []node
			var walk func(c *cobra.Command, prefix []string)
			walk = func(c *cobra.Command, prefix []string) {
				// Skip the root binary name; we want everything
				// after it (e.g. "wal stream", not "pg_hardstorage
				// wal stream").
				if len(prefix) > 0 {
					nodes = append(nodes, node{
						Path:           strings.Join(prefix, " "),
						Runnable:       c.Runnable(),
						HasSubcommands: c.HasSubCommands(),
						Hidden:         c.Hidden,
					})
				}
				for _, sub := range c.Commands() {
					walk(sub, append(prefix, sub.Name()))
				}
			}
			walk(cmd.Root(), nil)
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(nodes)
		},
	}
}
