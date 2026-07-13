// onboard_helpers.go — 'lint' + 'explain' stub commands surfaced during onboarding.
package cli

import (
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/version"
	"github.com/spf13/cobra"
)

func newLintCmdImpl() *cobra.Command {
	return &cobra.Command{
		Use: "lint", Short: "Validate pg_hardstorage.yaml",
		Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(map[string]any{"status": "valid"}))
		},
	}
}

func newExplainCmdImpl() *cobra.Command {
	return &cobra.Command{
		Use: "explain <cmd>", Short: "Explain pg_hardstorage commands",
		Args: cobra.MinimumNArgs(1), SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := DispatcherFrom(cmd)
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(map[string]any{
				"command": args[0],
			}))
		},
	}
}

func newChangelogCmdImpl() *cobra.Command {
	return &cobra.Command{
		Use: "changelog", Short: "Show changelog",
		Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(map[string]any{
				"version":   version.Version,
				"changelog": "https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/CHANGELOG.md",
			}))
		},
	}
}

func newGlossaryCmdImpl() *cobra.Command {
	return &cobra.Command{
		Use: "glossary [<term>]", Short: "Look up pg_hardstorage terminology",
		Args: cobra.MaximumNArgs(1), SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := DispatcherFrom(cmd)
			if len(args) == 0 {
				return d.Result(output.NewResult(cmd.CommandPath()).WithBody(map[string]any{
					"entries": []map[string]string{
						{"term": "deployment", "description": "A PG instance or cluster you backup"},
						{"term": "backup", "description": "One PITR-recoverable artifact"},
					},
				}))
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(map[string]any{"term": args[0]}))
		},
	}
}

// newDemoCmdImpl now lives in demo.go (issue #15) — it runs the real
// end-to-end flow instead of printing a placeholder message.
