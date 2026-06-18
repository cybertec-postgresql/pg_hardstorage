// wal_gaps.go — 'wal gaps' CLI verb: lists persisted WAL-gap records from leader-follow detections.
package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

// newWalGapsCmd implements `pg_hardstorage wal gaps <deployment>`.
// Lists the persisted WAL-gap records the leader-follow
// coordinator wrote when it detected a Patroni-failover gap.
//
// This is the operator-facing surface for the gap-state-file
// piece shipped alongside it. `doctor` already raises a
// `wal.gap_persistent` issue when any gap exists; this command
// is the inspection tool an operator opens to see the full
// history (last N detections, per timeline, with LSN ranges).
//
// Output is the standard pg_hardstorage.v1 envelope: text mode
// renders a tabular summary; JSON / NDJSON modes hand back the
// raw list for monitoring + scripts.
func newWalGapsCmd() *cobra.Command {
	var (
		repoURL  string
		timeline uint32
		limit    int
	)
	c := &cobra.Command{
		Use:   "gaps <deployment>",
		Short: "List persisted WAL-gap records (Patroni-failover diagnostics)",
		Long: `Lists the WAL-gap records the leader-follow coordinator
persisted when it detected a Patroni-failover gap (the new
leader's slot advanced past the agent's last confirmed LSN, so
PITR within the gap range is impossible from this repo).

Records are written to ` + "`" + `wal/<deployment>/gaps/<tli>-<ns>.json` + "`" + ` —
one per detection, append-only, so the forensic trail survives
agent restarts. Use this command to:

  - confirm whether a gap is outstanding (` + "`" + `pg_hardstorage doctor` + "`" + `
    raises a structured wal.gap_persistent issue when one exists)
  - inspect the LSN range PITR cannot bridge
  - verify which slot + role triggered each detection

Filters:

  --timeline N    only records on TLI N
  --limit   N    return at most N records (newest-first)`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWalGaps(cmd, args[0], repoURL, timeline, limit)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().Uint32Var(&timeline, "timeline", 0,
		"filter to records on this timeline only (0 = all)")
	c.Flags().IntVar(&limit, "limit", 0,
		"return at most this many records (0 = unbounded)")
	return c
}

func runWalGaps(cmd *cobra.Command, deployment, repoURL string, timeline uint32, limit int) error {
	d := DispatcherFrom(cmd)

	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	all, err := gapstate.New(sp).List(cmd.Context(), deployment)
	if err != nil {
		return output.NewError("wal.gaps_unreadable",
			fmt.Sprintf("wal gaps: %v", err)).Wrap(err)
	}

	// Filter by timeline if requested.
	filtered := all
	if timeline != 0 {
		filtered = filtered[:0]
		for _, r := range all {
			if r.Timeline == timeline {
				filtered = append(filtered, r)
			}
		}
	}
	// Total is the TRUE count of matching gaps; capture it BEFORE the
	// --limit truncation so an operator paging with --limit isn't told
	// there are only N gaps when there are more. Gaps are permanent WAL
	// loss — undercounting them could make an operator under-react.
	total := len(filtered)
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}

	body := walGapsBody{
		Schema:        "pg_hardstorage.wal.gaps.v1",
		Deployment:    deployment,
		Repo:          repoURL,
		TimelineMatch: timeline,
		Total:         total,
		Records:       filtered,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// walGapsBody is the v1-stable result for `wal gaps`.
type walGapsBody struct {
	Schema        string            `json:"schema"`
	Deployment    string            `json:"deployment"`
	Repo          string            `json:"repo"`
	TimelineMatch uint32            `json:"timeline_match,omitempty"`
	Total         int               `json:"total"`
	Records       []gapstate.Record `json:"records"`
}

// WriteText renders walGapsBody for text mode.
func (b walGapsBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if len(b.Records) == 0 {
		fmt.Fprintf(bw, "✓ no WAL gaps recorded for %s\n", b.Deployment)
		fmt.Fprintf(bw, "  Repo: %s\n", b.Repo)
		if b.TimelineMatch != 0 {
			fmt.Fprintf(bw, "  Timeline filter: %d\n", b.TimelineMatch)
		}
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	fmt.Fprintf(bw, "✗ %d WAL gap(s) recorded for %s\n", b.Total, b.Deployment)
	fmt.Fprintf(bw, "  Repo: %s\n", b.Repo)
	if b.TimelineMatch != 0 {
		fmt.Fprintf(bw, "  Timeline filter: %d\n", b.TimelineMatch)
	}
	if len(b.Records) < b.Total {
		fmt.Fprintf(bw, "  (showing the %d most recent of %d; raise --limit to see more)\n",
			len(b.Records), b.Total)
	}
	for i, r := range b.Records {
		fmt.Fprintf(bw, "\n  [%d] detected_at=%s\n", i+1,
			r.DetectedAt.UTC().Format(time.RFC3339))
		fmt.Fprintf(bw, "      timeline=%d  slot=%s  role=%s\n",
			r.Timeline, r.SlotName, r.SlotRole)
		fmt.Fprintf(bw, "      gap=%d bytes  range=%s..%s\n",
			r.GapBytes, r.GapStartLSN, r.GapEndLSN)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
