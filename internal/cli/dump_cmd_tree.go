// dump_cmd_tree.go — hidden '__dump-cmd-tree' verb: emits the cobra tree as JSON for coverage gates.
package cli

import (
	"encoding/json"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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
				// GroupGuard reports that Runnable is TRUE only
				// because hardenGroupCommands synthesised a RunE to
				// reject typo'd subcommands. The command itself does
				// no work: invoked bare it prints help and exits 0.
				//
				// Without this, every consumer of the dump has to
				// guess. The CLI coverage gate guessed wrong and
				// demanded a scenario for `kms`, `audit`, `repo` and
				// 30-odd other pure groups — which is why it reported
				// 41 uncovered leaves and had to be ignored.
				GroupGuard bool `json:"group_guard,omitempty"`
				// Flags is every flag valid on this command, local and
				// inherited. Emitted so a docs guard can check that a
				// `--flag` shown in a how-to actually exists on the
				// command it is shown with — prose is hand-written and
				// drifts, unlike the generated CLI reference.
				Flags []string `json:"flags,omitempty"`
			}
			var nodes []node
			var walk func(c *cobra.Command, prefix []string)
			walk = func(c *cobra.Command, prefix []string) {
				// Skip the root binary name; we want everything
				// after it (e.g. "wal stream", not "pg_hardstorage
				// wal stream").
				if len(prefix) > 0 {
					var flags []string
					c.Flags().VisitAll(func(f *pflag.Flag) {
						flags = append(flags, f.Name)
					})
					c.InheritedFlags().VisitAll(func(f *pflag.Flag) {
						flags = append(flags, f.Name)
					})
					sort.Strings(flags)
					nodes = append(nodes, node{
						Path:           strings.Join(prefix, " "),
						Runnable:       c.Runnable(),
						HasSubcommands: c.HasSubCommands(),
						Hidden:         c.Hidden,
						GroupGuard:     c.Annotations[groupGuardAnnotation] == "1",
						Flags:          flags,
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
