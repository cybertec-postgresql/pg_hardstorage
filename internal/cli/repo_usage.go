// repo_usage.go — CLI surface for the per-category object / byte usage rollup.
package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// newRepoUsageCmd implements `pg_hardstorage repo usage`. The
// operator-facing "where is my repo space going?" view.
//
// Implementation: walks the configured prefixes and tallies object
// count + bytes per category. Categories are repo-level abstractions
// (chunks, primary manifests, replica manifests, WAL, soft-deleted
// trash, audit) so the output stays meaningful regardless of how
// many deployments are in the repo.
//
// `--by-deployment` adds a per-deployment breakdown for chunks
// deferred — chunk objects don't carry their referencing deployment
// in the storage layer, so a real per-deployment chunk attribution
// requires reference walking. That's a enhancement; the v0.1
// cut is the high-level totals every operator needs first.
func newRepoUsageCmd() *cobra.Command {
	var repoURL string
	c := &cobra.Command{
		Use:          "usage <url>",
		Short:        "Report repository usage by category",
		Long:         `Walk the repository and report object count + bytes per category (chunks, manifests, WAL, trash, audit).`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if repoURL != "" && repoURL != args[0] {
					return output.NewError("usage.repo_conflict",
						"repo usage: --repo and the positional URL disagree").Wrap(output.ErrUsage)
				}
				repoURL = args[0]
			}
			return runRepoUsage(cmd, repoURL)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (positional <url> is also accepted)")
	return c
}

func runRepoUsage(cmd *cobra.Command, repoURL string) error {
	d := DispatcherFrom(cmd)
	// Positional-or-flag: guard the resolved value, not the flag.
	if repoURL == "" {
		return missingFlagErr(cmd, "--repo (or the first positional <url>)")
	}
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	body, err := scanRepoUsage(cmd.Context(), sp, repoURL)
	if err != nil {
		return err
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// scanRepoUsage walks the standard prefixes and tallies. Done as a
// separate function so future tooling (a TUI fleet view, a periodic
// metric exporter) can reuse the categorisation without going
// through the CLI dispatcher.
func scanRepoUsage(ctx context.Context, sp storage.StoragePlugin, repoURL string) (repoUsageBody, error) {
	body := repoUsageBody{URL: repoURL}

	// Each entry: prefix → bucket name. The categorisation logic
	// below looks at the FULL key (after listing under the bucket's
	// prefix) so we can split manifests/_replicas vs primary, and
	// manifests/_trash vs live.
	roots := []struct {
		prefix string
		assign func(key string) string // returns category name; "" = skip
	}{
		{prefix: "chunks/", assign: func(_ string) string { return "chunks" }},
		{prefix: "manifests/", assign: classifyManifest},
		{prefix: "wal/", assign: func(_ string) string { return "wal" }},
		{prefix: "audit/", assign: func(_ string) string { return "audit" }},
	}
	cats := map[string]*usageCat{}
	for _, r := range roots {
		for info, err := range sp.List(ctx, r.prefix) {
			if err != nil {
				return body, output.NewError("repo.usage.list_failed",
					fmt.Sprintf("repo usage: list %s: %v", r.prefix, err)).Wrap(err)
			}
			cat := r.assign(info.Key)
			if cat == "" {
				continue
			}
			c := cats[cat]
			if c == nil {
				c = &usageCat{Category: cat}
				cats[cat] = c
			}
			c.Objects++
			c.Bytes += info.Size
		}
	}

	// Sort by category name for deterministic output (and stable
	// JSON consumption — tests rely on order).
	out := make([]usageCat, 0, len(cats))
	for _, c := range cats {
		out = append(out, *c)
		body.TotalObjects += c.Objects
		body.TotalBytes += c.Bytes
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Category < out[j].Category })
	body.Categories = out
	return body, nil
}

// classifyManifest distinguishes between primary, replica, tombstone-
// marker, and trash manifests so the operator sees the layout
// breakdown rather than one undifferentiated "manifests" total.
//
// Today's tombstone strategy (per backup/manifest_store.go) keeps the
// soft-delete marker as `<manifest>.json.tombstone` BESIDE the primary
// manifest — there's no `manifests/_trash/` move. Without an explicit
// case here, those marker files were silently inflating the "manifests"
// count and bytes; an operator reading `repo usage` couldn't tell how
// much was queued for GC.
//
// `manifests-trash` is kept for the SPEC's forward-looking _trash/
// subprefix layout (in case a future tombstone strategy moves the
// manifest itself rather than dropping a sibling marker).
func classifyManifest(key string) string {
	switch {
	case strings.HasSuffix(key, ".json.tombstone"):
		return "manifests-tombstone"
	case strings.HasPrefix(key, "manifests/_replicas/"):
		return "manifests-replica"
	case strings.HasPrefix(key, "manifests/_trash/"):
		return "manifests-trash"
	}
	return "manifests"
}

type usageCat struct {
	Category string `json:"category"`
	Objects  int64  `json:"objects"`
	Bytes    int64  `json:"bytes"`
}

type repoUsageBody struct {
	URL          string     `json:"url"`
	Categories   []usageCat `json:"categories"`
	TotalObjects int64      `json:"total_objects"`
	TotalBytes   int64      `json:"total_bytes"`
}

// WriteText renders the per-category usage rollup as a tabular summary to w.
func (b repoUsageBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "repo usage — %s\n", b.URL)
	if len(b.Categories) == 0 {
		fmt.Fprintln(bw, "  empty repository")
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  CATEGORY\tOBJECTS\tBYTES")
	for _, c := range b.Categories {
		fmt.Fprintf(tw, "  %s\t%d\t%s\n", c.Category, c.Objects, humanBytes(c.Bytes))
	}
	fmt.Fprintf(tw, "  --\t--\t--\n")
	fmt.Fprintf(tw, "  TOTAL\t%d\t%s\n", b.TotalObjects, humanBytes(b.TotalBytes))
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
