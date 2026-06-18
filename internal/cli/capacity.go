// capacity.go — CLI surface for repo capacity forecasting reports.
package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/capacity"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// newRealCapacityCmd implements `pg_hardstorage capacity report`.
//
// v0.1 fits a linear least-squares trend to the manifest history. The
// model is honest about its limits — bursty workloads get a low R²
// and a "low" confidence label rather than a misleadingly precise
// projection. Non-linear models land alongside the time-series
// store the SLO and cost subsystems share.
func newRealCapacityCmd() *cobra.Command {
	var (
		repoURL string
		horizon time.Duration
	)
	c := &cobra.Command{
		Use:   "report",
		Short: "Projected repository size and WAL volume",
		Long: `Project the repository's size at now + horizon by fitting a
linear least-squares trend to the manifest history (StartedAt vs
cumulative logical bytes).

The result includes:

  - Total bytes_per_day (slope of the global fit, weighted by each
    deployment's current footprint)
  - Projected bytes at the horizon (current + slope*horizon)
  - R² and a categorical confidence (high|medium|low|insufficient)
  - Per-deployment slice with each deployment's slope and R²

A repo with fewer than 3 committed manifests gets a structured
"insufficient" confidence with a note — better than a noisy fit
through two points.

Default horizon: 90d. Override with --horizon (Go duration syntax;
24h, 720h for 30d, etc).`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCapacityReport(cmd, capacityReportOptions{
				repoURL: repoURL,
				horizon: horizon,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().DurationVar(&horizon, "horizon", capacity.DefaultHorizon,
		"projection horizon (Go duration syntax; default 90d)")
	return c
}

type capacityReportOptions struct {
	repoURL string
	horizon time.Duration
}

func runCapacityReport(cmd *cobra.Command, opts capacityReportOptions) error {
	d := DispatcherFrom(cmd)
	_, sp, err := openRepo(cmd.Context(), opts.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	r, err := capacity.Project(cmd.Context(), sp, opts.repoURL, capacity.ProjectOptions{
		Horizon: opts.horizon,
	})
	if err != nil {
		return output.NewError("capacity.project_failed",
			fmt.Sprintf("capacity report: %v", err)).Wrap(err)
	}

	body := capacityReportBody{Report: r}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// capacityReportBody wraps capacity.Report so the JSON renderer
// emits the report's own schema as the result body.
type capacityReportBody struct {
	*capacity.Report
}

// MarshalJSON emits the embedded capacity.Report so consumers see its own
// v1 schema as the result body.
func (b capacityReportBody) MarshalJSON() ([]byte, error) {
	return marshalIndentedJSON(b.Report)
}

// WriteText renders the capacity report as human-readable text to w, including
// per-deployment rollups when present.
func (b capacityReportBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "capacity report — %s\n", b.RepoURL)
	fmt.Fprintf(bw, "  Generated:    %s\n", b.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "  Horizon:      %s (at %s)\n", b.Horizon, b.HorizonAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "  Confidence:   %s\n", b.Confidence)
	if b.Note != "" {
		fmt.Fprintf(bw, "  Note:         %s\n", b.Note)
	}
	if b.Confidence == "insufficient" {
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	fmt.Fprintf(bw, "  Samples:      %d\n", b.SamplesUsed)
	fmt.Fprintf(bw, "  Current:      %s\n", humanBytes(b.CurrentBytes))
	fmt.Fprintf(bw, "  Per day:      %s\n", humanBytes(b.BytesPerDay))
	fmt.Fprintf(bw, "  Projected:    %s (Δ %s)\n",
		humanBytes(b.ProjectedBytes), humanBytes(b.ProjectedDeltaBytes))
	fmt.Fprintf(bw, "  Fit (R²):     %.3f\n", b.RSquared)
	if len(b.PerDeployment) > 0 {
		fmt.Fprintf(bw, "\n  Per deployment:\n")
		tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "    DEPLOYMENT\tBACKUPS\tCURRENT\tPER-DAY\tPROJECTED\tR²")
		for _, dp := range b.PerDeployment {
			fmt.Fprintf(tw, "    %s\t%d\t%s\t%s\t%s\t%.3f\n",
				dp.Name, dp.BackupCount,
				humanBytes(dp.CurrentBytes),
				humanBytes(dp.BytesPerDay),
				humanBytes(dp.ProjectedBytes),
				dp.RSquared)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// marshalIndentedJSON is a tiny helper used by both cost.WriteJSON and
// capacity report. We don't import encoding/json into every CLI file
// just to do an indented marshal — this keeps the call site small.
func marshalIndentedJSON(v any) ([]byte, error) {
	type marshaler interface {
		Marshal() ([]byte, error)
	}
	if m, ok := v.(marshaler); ok {
		return m.Marshal()
	}
	// Fallback: only used if a future caller passes something that
	// doesn't implement Marshal — we'd then route through encoding/json
	// proper. Cobra's tree won't hit this in v0.1.
	return nil, fmt.Errorf("marshalIndentedJSON: type does not implement Marshal()")
}
