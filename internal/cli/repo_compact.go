// repo_compact.go — 'repo compact' CLI verb: compact small chunks into pack files (engine deferred; surface consistent with repo gc/audit).
package cli

import (
	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// newRepoCompactCmd implements `pg_hardstorage repo compact`. The
// compaction ENGINE is deferred (see docs/SPEC.md) — RunE returns the
// not-implemented scaffold error — but the command SURFACE matches its
// repo siblings (gc/audit): the repo URL is taken as the <url> positional
// OR via --repo, with --apply for the dry-run/execute split.
//
// Why declare the real shape now instead of leaving it a bare stub: the
// previous stub accepted no --repo flag and no positional, so every
// `repo compact --repo <url>` (the natural form, identical to repo gc) was
// rejected — by the operator-facing LLM command-validator as an "unknown
// flag", and inconsistently with the sibling verbs that DO take --repo.
// Pinning the contract here keeps scripts and the validator consistent
// across the repo verbs and means the flag surface won't shift when the
// engine lands.
func newRepoCompactCmd() *cobra.Command {
	var (
		repoURL string
		apply   bool
	)
	c := &cobra.Command{
		Use:   "compact <url>",
		Short: "Compact small chunks into pack files (deferred)",
		Long: `compact rewrites many small chunk objects into a smaller number of
pack files, cutting per-object storage overhead and the request count
that object stores bill for.

The compaction engine is deferred (docs/SPEC.md); this command is a
scaffold that pins the eventual surface. It accepts the repo URL the
same way as ` + "`repo gc`" + ` and ` + "`repo audit`" + ` — as the <url>
positional OR via --repo — and follows the same dry-run-by-default /
--apply posture, so the contract is stable for scripts before the
engine ships.`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if repoURL != "" && repoURL != args[0] {
					return output.NewError("usage.repo_conflict",
						"repo compact: --repo and the positional URL disagree").Wrap(output.ErrUsage)
				}
				repoURL = args[0]
			}
			return output.NewError(
				"notimpl.compact",
				"`pg_hardstorage repo compact` is not yet implemented; this is a scaffold tracking the design plan",
			).WithSuggestion(&output.Suggestion{
				Human:  "see the design specification for what this command will do",
				DocURL: "docs/SPEC.md",
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (positional <url> is also accepted)")
	c.Flags().BoolVar(&apply, "apply", false,
		"actually rewrite chunks into pack files (default: dry-run)")
	return c
}
