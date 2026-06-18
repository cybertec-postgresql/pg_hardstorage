// wal_audit.go — CLI surface for auditing WAL segment continuity and timeline coverage.
package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// newWalAuditCmd implements `pg_hardstorage wal audit <deployment>`.
//
// The plan calls this out as a deliverable: "Periodic job lists
// segments in repo, asserts no LSN holes; gaps trigger auto-refetch
// where possible and `wal_gap_detected` alert."
//
// Two framings of the same primitive (the gap detector that powers
// `wal list --gaps-only`):
//
//   - `wal list` is the diagnostic. "What WAL do I have? Are there
//     gaps?" Result body always returns; exit 0 even on findings.
//
//   - `wal audit` is the maintenance. "Run me hourly from cron;
//     alarm if there's a gap." Findings flip the exit code to
//     ExitVerifyFailed and emit a `wal.gap_detected` audit-chain
//     entry. Same posture as `repo scrub` (cron) vs `repair scrub`
//     (diagnostic).
//
// The split lets operators wire `wal audit` into their schedule
// without having to remember the `wal list --gaps-only` incantation,
// and keeps `wal list` as the read-only operator query when
// investigating a known issue.
func newWalAuditCmd() *cobra.Command {
	var (
		repoURL  string
		timeline uint32
	)
	c := &cobra.Command{
		Use:   "audit <deployment>",
		Short: "Detect WAL gaps and alarm on findings (cron-friendly)",
		Long: `wal audit walks the repo's wal/<deployment>/<TLI>/ tree and
asserts every committed segment fits exactly after its predecessor
on the same timeline. A non-contiguous range is a gap — the "WAL
got lost between agent disconnect and slot recreate" case the plan
calls out.

Findings flip the exit code to ExitVerifyFailed (9) so cron-driven
audits alarm. A ` + "`wal.gap_detected`" + ` audit-chain entry is
appended on every gap-finding run — bit-rot-style: rare enough to
be signal, not noise.

For the read-only "what segments do I have?" query, use
` + "`wal list <deployment> --gaps-only`" + ` — same gap-detection
primitive, no exit-code flip, no audit emission.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWalAudit(cmd, args[0], repoURL, timeline)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL (file://, s3://, ...) — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().Uint32Var(&timeline, "timeline", 0,
		"only audit segments on this timeline (0 = all timelines)")
	return c
}

func runWalAudit(cmd *cobra.Command, deployment, repoURL string, tliFilter uint32) error {
	d := DispatcherFrom(cmd)
	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	startedAt := time.Now().UTC()
	segs, err := scanWALSegments(cmd.Context(), sp, deployment, tliFilter)
	if err != nil {
		return err
	}
	gaps := findGaps(segs)
	timelines := summariseTimelines(segs)
	stoppedAt := time.Now().UTC()

	var totalMissing uint64
	for _, g := range gaps {
		totalMissing += g.MissingCount
	}

	body := walAuditBody{
		Schema:         "pg_hardstorage.wal.audit.v1",
		Deployment:     deployment,
		Repo:           repoURL,
		TimelineFilter: tliFilter,
		Timelines:      timelines,
		SegmentCount:   len(segs),
		GapCount:       len(gaps),
		Gaps:           gaps,
		MissingTotal:   totalMissing,
		StartedAt:      startedAt,
		StoppedAt:      stoppedAt,
		DurationMS:     stoppedAt.Sub(startedAt).Milliseconds(),
	}

	if len(gaps) == 0 {
		return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
	}

	// Findings → audit emission + ExitVerifyFailed. Best-effort
	// audit append (a failure here doesn't change the verdict; the
	// JSON result already carries the per-gap detail).
	emitWALGapAudit(cmd.Context(), sp, repoMeta, repoURL, deployment, &body)

	// Render a one-line-per-gap summary into the error message so
	// text-mode operators see what's missing without needing the
	// JSON body. Shape mirrors anomaly.detected's error.
	var lines []string
	for _, g := range gaps {
		lines = append(lines, fmt.Sprintf("TLI %d: segments #%d..#%d (%d missing)",
			g.Timeline, g.StartSegment, g.EndSegment, g.MissingCount))
	}
	return output.NewError("verify.wal_gap_detected",
		fmt.Sprintf("wal audit: %d gap(s) totalling %d missing segment(s)\n%s",
			len(gaps), totalMissing, strings.Join(lines, "\n"))).
		WithSuggestion(&output.Suggestion{
			Human:   "WAL is missing between segments. If a slot was dropped during a network blip, take a fresh full backup; gaps within a single timeline can also indicate a partial-archive failure.",
			Command: fmt.Sprintf("pg_hardstorage wal list %s --repo %s --gaps-only", deployment, repoURL),
			DocURL:  "docs/runbooks/wal-gap-detected.md",
		})
}

// emitWALGapAudit writes ONE wal.gap_detected audit event per audit
// run. We deliberately emit once-per-run (not once-per-gap) — a
// single event with the per-gap detail as Body keeps the audit chain
// from getting noisy when many gaps appear at once (e.g. after a
// long network partition). Best-effort; we ignore the error so the
// CLI verdict path never depends on audit-chain availability.
func emitWALGapAudit(ctx context.Context, sp storage.StoragePlugin, repoMeta *repo.Metadata, repoURL, deployment string, body *walAuditBody) {
	store := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	gapBodies := make([]map[string]any, 0, len(body.Gaps))
	for _, g := range body.Gaps {
		gapBodies = append(gapBodies, map[string]any{
			"timeline":      g.Timeline,
			"start_segment": g.StartSegment,
			"end_segment":   g.EndSegment,
			"missing_count": g.MissingCount,
		})
	}
	store.AppendOrLog(ctx, &audit.Event{
		Action: "wal.gap_detected",
		Subject: audit.Subject{
			Repo:       repoURL,
			Deployment: deployment,
		},
		Timestamp: time.Now().UTC(),
		Body: map[string]any{
			"deployment":      deployment,
			"timeline_filter": body.TimelineFilter,
			"segment_count":   body.SegmentCount,
			"gap_count":       body.GapCount,
			"missing_total":   body.MissingTotal,
			"gaps":            gapBodies,
			"duration_ms":     body.DurationMS,
		},
	})
}

// walAuditBody is the v1-stable Result body. Cron-friendly: every
// counter the operator might graph is a top-level integer;
// duration_ms is a fixed unit; started/stopped_at are time.Time
// (RFC 3339).
type walAuditBody struct {
	Schema         string `json:"schema"`
	Deployment     string `json:"deployment"`
	Repo           string `json:"repo,omitempty"`
	TimelineFilter uint32 `json:"timeline_filter,omitempty"`

	Timelines    []walTimelineSummary `json:"timelines"`
	SegmentCount int                  `json:"segment_count"`
	GapCount     int                  `json:"gap_count"`
	Gaps         []walGap             `json:"gaps,omitempty"`
	MissingTotal uint64               `json:"missing_total"`

	StartedAt  time.Time `json:"started_at"`
	StoppedAt  time.Time `json:"stopped_at"`
	DurationMS int64     `json:"duration_ms"`
}

// WriteText renders the WAL audit — per-timeline summary plus any detected
// gaps — as human-readable text to w.
func (b walAuditBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "wal audit — %s\n", b.Deployment)
	if b.TimelineFilter != 0 {
		fmt.Fprintf(bw, "  Timeline filter: %d\n", b.TimelineFilter)
	}
	fmt.Fprintf(bw, "  Segments scanned: %d\n", b.SegmentCount)
	if len(b.Timelines) > 0 {
		fmt.Fprintln(bw, "  Timelines:")
		for _, t := range b.Timelines {
			fmt.Fprintf(bw, "    TLI %d: %d segments (#%d..#%d)\n",
				t.Timeline, t.SegmentCount, t.LowestSegment, t.HighestSegment)
		}
	}
	fmt.Fprintf(bw, "  Duration:        %d ms\n", b.DurationMS)
	if b.GapCount == 0 {
		fmt.Fprintln(bw, "  ✓ no gaps detected")
	} else {
		fmt.Fprintf(bw, "  ✗ %d gap(s), %d total missing segment(s):\n",
			b.GapCount, b.MissingTotal)
		for _, g := range b.Gaps {
			fmt.Fprintf(bw, "    TLI %d: segments #%d..#%d (%d missing)\n",
				g.Timeline, g.StartSegment, g.EndSegment, g.MissingCount)
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
