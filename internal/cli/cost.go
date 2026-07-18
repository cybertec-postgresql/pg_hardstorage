// cost.go — CLI surface for storage cost estimation reports.
package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cost"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// newRealCostCmd implements `pg_hardstorage cost report`.
//
// What ships in v0.1:
//   - Total physical bytes (chunks/manifests/wal/audit), with a
//     monthly-USD estimate at $0.023/GB-month default (overridable).
//   - Per-deployment manifest, WAL, and logical bytes.
//   - Backup count per deployment.
//
// What deliberately doesn't ship:
//   - Per-deployment *chunk* attribution — chunks are content-addressed
//     and dedup-shared, requiring a reference-graph walk to apportion
//     multi-referenced chunks. The total chunk_bytes line shows the
//     repo-wide cost; the operator's "did this deployment fall off a
//     cliff?" question is answered by manifest + WAL trend lines that
//     ARE per-deployment exact.
//   - Time-windowed slicing (`--since 30d`). Cost trends need
//     historical sampling, which the SLO + capacity reports collect.
//   - Multi-cloud price tables (Azure / GCS / on-prem). The single
//     `--price-per-gb-month` flag is the v0.1 escape hatch.
func newRealCostCmd() *cobra.Command {
	var (
		repoURL   string
		priceFlag float64
	)
	c := &cobra.Command{
		Use:   "report",
		Short: "Per-deployment / per-tenant repository cost",
		Long: `Walk the repository and report bytes consumed by category, with
a monthly-USD estimate.

Total physical bytes are exact; the per-deployment slice covers
manifest + WAL bytes (exact) and logical / pre-dedup bytes from
every committed manifest. Per-deployment chunk attribution lands
in — chunks are content-addressed and dedup-shared, so a
precise apportionment requires walking the manifest reference
graph.

Default price is $0.023/GB-month (AWS S3 Standard, us-east-1).
Override with --price-per-gb-month for other backends or rates.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCostReport(cmd, costReportOptions{
				repoURL: repoURL,
				price:   priceFlag,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL — must already exist (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().Float64Var(&priceFlag, "price-per-gb-month", cost.DefaultPricePerGBMonth,
		"USD per GB per month (default: AWS S3 Standard us-east-1)")
	return c
}

type costReportOptions struct {
	repoURL string
	price   float64
}

func runCostReport(cmd *cobra.Command, opts costReportOptions) error {
	d := DispatcherFrom(cmd)
	if !finiteFloat(opts.price) || opts.price < 0 {
		return output.NewError("usage.bad_flag",
			"cost report: --price-per-gb-month must be finite and >= 0").Wrap(output.ErrUsage)
	}
	_, sp, err := openRepo(cmd.Context(), opts.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	r, err := cost.Compute(cmd.Context(), sp, opts.repoURL, opts.price)
	if err != nil {
		return output.NewError("cost.compute_failed",
			fmt.Sprintf("cost report: %v", err)).Wrap(err)
	}

	body := costReportBody{Report: r}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// costReportBody wraps the cost.Report so we can attach a WriteText
// hook without polluting the cost package with cli-only concerns.
type costReportBody struct {
	*cost.Report
}

// MarshalJSON forwards to the embedded Report so the JSON shape matches
// the schema string exactly. Without this the JSON renderer would
// double-wrap under a "report" key.
func (b costReportBody) MarshalJSON() ([]byte, error) {
	return b.Report.Marshal()
}

// WriteText renders the cost report — total spend plus per-category breakdown —
// as human-readable text to w.
func (b costReportBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "cost report — %s\n", b.RepoURL)
	fmt.Fprintf(bw, "  Total physical:    %s\n", humanBytes(b.TotalPhysicalBytes))
	fmt.Fprintf(bw, "  Estimated monthly: $%.2f USD (at $%.4f/GB-month)\n",
		b.EstimatedMonthlyUSD, b.PricePerGBMonth)
	fmt.Fprintf(bw, "\n  Breakdown:\n")
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "    chunks\t%s\n", humanBytes(b.ChunkBytes))
	fmt.Fprintf(tw, "    manifests\t%s\n", humanBytes(b.ManifestBytes))
	fmt.Fprintf(tw, "    wal\t%s\n", humanBytes(b.WALBytes))
	fmt.Fprintf(tw, "    audit\t%s\n", humanBytes(b.AuditBytes))
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(b.Deployments) > 0 {
		fmt.Fprintf(bw, "\n  Per deployment:\n")
		tw = tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "    DEPLOYMENT\tBACKUPS\tMANIFESTS\tWAL\tLOGICAL")
		for _, dc := range b.Deployments {
			fmt.Fprintf(tw, "    %s\t%d\t%s\t%s\t%s\n",
				dc.Name, dc.BackupCount,
				humanBytes(dc.ManifestBytes),
				humanBytes(dc.WALBytes),
				humanBytes(dc.LogicalBytes))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(bw, "\n  No deployments yet.\n")
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
