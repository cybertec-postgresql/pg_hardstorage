// fleet.go — CLI surface for cross-deployment search over the manifest index.
package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/fleet/search"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// newRealFleetCmd builds the `fleet` command tree. v0.1 ships
// `fleet search` against a single repository; the multi-repo "every
// repo I have configured" walk lands when the control plane lands.
func newRealFleetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "fleet <search>",
		Short: "Fleet-wide queries across deployments and backups",
		Long: `Fleet operations.

v0.1 ships fleet search — point at a repository, give a key:value
query, and get back every matching backup. Multi-repo aggregation
arrives with the control plane.`,
	}
	c.AddCommand(newFleetSearchCmd())
	return c
}

func newFleetSearchCmd() *cobra.Command {
	var (
		repoURL string
		query   string
		limit   int
	)
	c := &cobra.Command{
		Use:   "search --query '<expr>' --repo <url>",
		Short: "Search every deployment's manifests in one repo",
		Long: `Walk every deployment's committed manifests in the given
repository and return those matching the AND-of-key:value query.

Supported keys:
  deployment:<name>
  tenant:<name>
  type:<full|incremental>
  pg_version:<int>
  timeline:<int>
  since:<7d|24h|RFC3339>
  before:<7d|24h|RFC3339>

Examples:
  fleet search --query 'deployment:db1 type:full since:7d' --repo s3://...
  fleet search --query 'pg_version:17 timeline:3'         --repo file:///srv/...

Tombstoned (soft-deleted) manifests are excluded. Manifests are
signature-verified with the local keyring (as list does); any that
cannot be read or verified are counted as "unreadable" and flagged,
because the hit list is then incomplete.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFleetSearch(cmd, fleetSearchOptions{
				repoURL: repoURL,
				query:   query,
				limit:   limit,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	// The `-q` shorthand is reserved by the persistent
	// `--quiet` flag on the root command; `--query` keeps
	// the long form only to avoid the collision (caught
	// during the doc-gen walk; pre-fix, `pg_hardstorage
	// fleet search --help` panicked at flag merge time).
	c.Flags().StringVar(&query, "query", "",
		"key:value AND query (required)")
	_ = c.MarkFlagRequired("query")
	c.Flags().IntVar(&limit, "limit", 0,
		"cap on returned hits (default: no limit)")
	return c
}

type fleetSearchOptions struct {
	repoURL string
	query   string
	limit   int
}

func runFleetSearch(cmd *cobra.Command, opts fleetSearchOptions) error {
	d := DispatcherFrom(cmd)
	if strings.TrimSpace(opts.query) == "" {
		return output.NewError("usage.missing_flag",
			"fleet search: --query is required").
			WithSuggestion(&output.Suggestion{
				Human: "example: --query 'deployment:db1 type:full since:7d'",
			}).Wrap(output.ErrUsage)
	}
	q, err := search.Parse(opts.query)
	if err != nil {
		return output.NewError("usage.bad_query",
			fmt.Sprintf("fleet search: %v", err)).Wrap(output.ErrUsage)
	}

	// Every committed manifest is signed; without the operator's
	// verifier each one fails verification and the search answers
	// "0 hits" on a populated repository. Same keyring `list` uses.
	verifier, err := loadVerifier()
	if err != nil {
		return err
	}

	_, sp, err := openRepo(cmd.Context(), opts.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	res, err := search.Search(cmd.Context(), sp, q, search.SearchOptions{
		Limit:    opts.limit,
		Verifier: verifier,
	})
	if err != nil {
		return output.NewError("fleet.search_failed",
			fmt.Sprintf("fleet search: %v", err)).Wrap(err)
	}

	body := fleetSearchBody{
		RepoURL:    opts.repoURL,
		Query:      q.String(),
		Hits:       res.Hits,
		Count:      len(res.Hits),
		Unreadable: res.Unreadable,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

type fleetSearchBody struct {
	RepoURL string       `json:"repo_url"`
	Query   string       `json:"query"`
	Hits    []search.Hit `json:"hits"`
	Count   int          `json:"count"`
	// Unreadable counts manifests that failed to read or verify and so
	// could not be matched: non-zero means the hit list may be
	// incomplete. Always emitted so a JSON consumer can tell "0 hits"
	// from "0 hits because nothing verified".
	Unreadable int `json:"unreadable"`
}

// WriteText renders the fleet search hits as a tabular summary to w.
func (b fleetSearchBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "fleet search — %s\n  query: %s\n  hits:  %d\n", b.RepoURL, b.Query, b.Count)
	if b.Unreadable > 0 {
		fmt.Fprintf(bw, "  WARNING: %d manifest(s) could not be read or verified and were not searched; results may be incomplete\n", b.Unreadable)
	}
	if b.Count == 0 {
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  DEPLOYMENT\tBACKUP\tTYPE\tPG\tTLI\tSTARTED\tFILES")
	for _, h := range b.Hits {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%d\t%s\t%d\n",
			h.Deployment, h.BackupID, h.Type, h.PGVersion, h.Timeline,
			h.StartedAt.Format(time.RFC3339), h.Files)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
