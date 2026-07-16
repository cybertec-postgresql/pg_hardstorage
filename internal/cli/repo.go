// repo.go — 'repo' CLI verb parent (init/check/gc/usage/audit/replicate/set-mode/scrub/wipe/bundle).
package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// newRealRepoCmd is the in-development repo command tree. Slice 3 only
// implements `repo init`; the others stay stubs and accrete as we go.
func newRealRepoCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "repo <init|check|gc|compact|usage|audit|replicate|set-mode|scrub|wipe|bundle>",
		Short: "Manage the repository",
	}
	c.AddCommand(
		newRepoInitCmd(),
		newRepoCheckCmd(),
		newRepoGCCmd(),
		newRepoUsageCmd(),
		newRepoAuditCmd(),
		newRepoCompactCmd(),
		newRepoReplicateCmd(),
		newRepoSetModeCmd(),
		newRepoScrubCmd(),
		newRepoWipeCmd(),
		newRepoBundleCmd(),
	)
	return c
}

func newRepoInitCmd() *cobra.Command {
	var (
		repoURL       string
		wormMode      string
		wormRetention string
		compression   string
	)
	c := &cobra.Command{
		Use:   "init <url>",
		Short: "Create a new repository at the given URL",
		Long: `Initialise a new pg_hardstorage repository at the given URL.

Supported schemes: file://<absolute-path>, s3://<bucket>/<prefix>

The operation is atomic and race-safe: if two processes init the same
URL concurrently, exactly one wins and the other receives a conflict
error.

WORM (write-once-read-many):

    --worm-mode {compliance|governance} \
    --worm-retention <duration>     # 7y, 30d, 8760h

Records a retention policy in HSREPO that propagates to every
committed object's PUT (chunks, manifests, replicas, audit events).
Compliance mode is the regulatory-grade posture (even root
credentials cannot delete before the deadline). Governance mode
allows IAM principals with the BypassGovernance permission to
delete. WORM is set at init time only — flipping it on later
would produce a mixed-fleet situation operators can't reason about.

Retention units: y (365-day years), d (days), h (hours), m (minutes).

Compression:

    --compression {fast|balanced|max}

Picks the zstd encoder level for new chunks.  Profiling under a
write-heavy workload (10 GB pgbench seed + sustained UPDATE load)
showed the original default ("balanced", ~zstd level 7) burned
~40% of pg_hardstorage CPU.

  fast     ~zstd level 3.  Halves zstd CPU; ~10-15% larger on disk.
           Recommended for write-heavy clusters where wal-stream
           CPU matters more than disk bytes.
  balanced ~zstd level 7.  The default.  Sweet spot for
           the median operator.
  max      ~zstd level 11.  2-3x more CPU than balanced; ~5%
           smaller on disk.  Archive-tier backups read rarely.

Set at init time and not changed after.  A repo holding a mix of
levels still reads back fine — the decoder handles every level —
but the operator's "what does my CPU/disk trade-off look like?"
answer is more legible when the level is stable across the repo.`,
		// Accept the URL as a positional OR via --repo, so it matches
		// every other repo verb (repo check/usage/gc/scrub all take
		// --repo). Previously only a bare positional was accepted and
		// `repo init --repo <url>` failed with "unknown flag".
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			url := repoURL
			if len(args) == 1 {
				if url != "" && url != args[0] {
					return output.NewError("usage.bad_args",
						"repo init: URL given both as positional and --repo; pass it once").Wrap(output.ErrUsage)
				}
				url = args[0]
			}
			if url == "" {
				return output.NewError("usage.missing_arg",
					"repo init: repository URL is required (positional <url> or --repo <url>)").
					WithSuggestion(&output.Suggestion{
						Human:   "example: pg_hardstorage repo init file:///srv/pg_hardstorage/repo",
						Command: "pg_hardstorage repo init --repo file:///srv/pg_hardstorage/repo",
					}).Wrap(output.ErrUsage)
			}
			return runRepoInit(cmd, url, wormMode, wormRetention, compression)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL to create (alternative to the positional <url>)")
	c.Flags().StringVar(&wormMode, "worm-mode", "",
		"WORM retention mode: compliance | governance (set with --worm-retention)")
	c.Flags().StringVar(&wormRetention, "worm-retention", "",
		"WORM retention duration: e.g. 7y, 30d, 8760h (required with --worm-mode)")
	c.Flags().StringVar(&compression, "compression", "",
		"zstd encoder level: fast | balanced | max (default: balanced)")
	return c
}

func runRepoInit(cmd *cobra.Command, url, wormMode, wormRetention, compression string) error {
	d := DispatcherFrom(cmd)

	worm, err := repo.MakeWORMPolicy(wormMode, wormRetention)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("repo init: %v", err)).Wrap(output.ErrUsage)
	}

	level := repo.CompressionLevel(compression)
	if err := level.Validate(); err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("repo init: %v", err)).Wrap(output.ErrUsage)
	}

	res, err := repo.Init(cmd.Context(), repo.InitOptions{
		URL:         url,
		WORM:        worm,
		Compression: level,
	})
	if err != nil {
		return mapRepoInitError(url, err)
	}

	body := repoInitBody{
		URL:       res.URL,
		ID:        res.ID,
		Schema:    res.Schema,
		CreatedAt: res.Metadata.CreatedAt,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// mapRepoInitError translates an error from repo.Init into a structured
// *output.Error with the right code and a useful Suggestion.
func mapRepoInitError(url string, err error) error {
	if errors.Is(err, repo.ErrAlreadyExists) {
		return output.NewError("conflict.repo_exists",
			fmt.Sprintf("a repository already exists at %s", url)).
			WithSuggestion(&output.Suggestion{
				Human:  "use a different URL or open the existing repo with `repo check`",
				DocURL: "docs/SPEC.md",
			}).Wrap(err)
	}
	if errors.Is(err, repo.ErrNotARepo) {
		return output.NewError("notfound.repo", err.Error()).Wrap(err)
	}
	if errors.Is(err, storage.ErrUnknownScheme) {
		return output.NewError("usage.unknown_scheme", err.Error()).
			WithSuggestion(&output.Suggestion{
				Human: fmt.Sprintf("registered schemes: %v", storage.Schemes()),
			}).Wrap(output.ErrUsage)
	}
	return output.NewError("internal", err.Error()).Wrap(err)
}

// repoInitBody is the typed body for `repo init`'s success Result.
type repoInitBody struct {
	URL       string `json:"url"`
	ID        string `json:"id"`
	Schema    string `json:"schema"`
	CreatedAt string `json:"created_at"`
}

// WriteText is the text-renderer hook.
func (r repoInitBody) WriteText(w io.Writer) error {
	_, err := fmt.Fprintf(w,
		"✓ Repository initialised\n  URL:    %s\n  ID:     %s\n  Schema: %s\n  Created: %s",
		r.URL, r.ID, r.Schema, r.CreatedAt)
	return err
}
