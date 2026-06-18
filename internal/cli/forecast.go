// forecast.go — CLI surface for storage growth forecasts (compact + markdown).
package cli

import (
	stdjson "encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/forecast"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// newForecastCmd implements `pg_hardstorage forecast` — the
// capacity-planning + cost-projection report.
//
// Operationally, the forecast answers: "given how the fleet has
// grown over the last N days, where will we be in 30/90/365 days
// and what will it cost?".  Different from the other diagnostic
// surfaces:
//
//   - doctor       — present-state health (host / config / connectivity)
//   - repo audit   — present-state inventory (counts / breakdowns)
//   - compliance   — historical events (audit-log rollup over a window)
//   - forecast     — historical trends → forward projections
//
// Read-only by construction; safe at any cadence including against
// WORM-locked repos.
func newForecastCmd() *cobra.Command {
	var (
		repoURL         string
		deployment      string
		baselineWindow  string
		horizons        []string
		pricePerGBMonth float64
		currency        string
		pricingModel    string
		format          string
		skipFleet       bool
		skipAnomalies   bool
	)
	c := &cobra.Command{
		Use:   "forecast <url>",
		Short: "Capacity-planning + cost-projection report",
		Long: `forecast walks the repository's manifest history, fits an
ordinary-least-squares linear regression to the per-day
cumulative-bytes series for each deployment, and projects forward
to the configured horizons (defaults: 30d / 90d / 365d).  The
report includes per-deployment forecasts, a fleet-wide rollup, an
optional cost projection, and growth-anomaly detection.

Window:
  --baseline-window DURATION  default 90d.  Backups stopped before
                              this window don't influence the
                              regression but still count toward
                              the manifest total.
  --horizon DURATION          repeatable; defaults to 30d, 90d, 365d.

Cost projection (opt-in):
  --price-per-gb-month F      monthly $/GiB rate.  We multiply the
                              fleet projection by this; cloud
                              tariffs include request fees + egress
                              + lifecycle classes that the
                              operator's billing system handles.
  --currency LABEL            default USD.
  --pricing-model LABEL       free-form ("s3-standard",
                              "s3-ia", "gcs-coldline"); recorded
                              in the report for transparency.

Output formats:
  --format json     (default) — JSON body, the v1 contract.
  --format markdown — forensics-grade GFM Markdown rendering.
                      Same shape as compliance report.

Skip flags:
  --no-fleet        suppress the fleet rollup
  --no-anomalies    suppress the anomaly-detection pass

The forecast is a planning tool, not a guarantee.  Confidence per
deployment is reported (high/medium/low/insufficient) so the
operator can apply their own quality threshold.  Read-only; safe
at any cadence.`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if repoURL != "" && repoURL != args[0] {
					return output.NewError("usage.repo_conflict",
						"forecast: --repo and the positional URL disagree").Wrap(output.ErrUsage)
				}
				repoURL = args[0]
			}
			return runForecast(cmd, forecastFlags{
				repoURL:         repoURL,
				deployment:      deployment,
				baselineWindow:  baselineWindow,
				horizons:        horizons,
				pricePerGBMonth: pricePerGBMonth,
				currency:        currency,
				pricingModel:    pricingModel,
				format:          format,
				skipFleet:       skipFleet,
				skipAnomalies:   skipAnomalies,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (positional <url> also accepted)")
	c.Flags().StringVar(&deployment, "deployment", "",
		"restrict the forecast to one deployment (fleet rollup still covers all)")
	c.Flags().StringVar(&baselineWindow, "baseline-window", "",
		"baseline window duration (e.g. 90d, 30d, 168h); default 90d")
	c.Flags().StringSliceVar(&horizons, "horizon", nil,
		"projection horizon (repeatable, e.g. --horizon 30d --horizon 365d); default: 30d, 90d, 365d")
	c.Flags().Float64Var(&pricePerGBMonth, "price-per-gb-month", 0,
		"monthly $/GiB rate for the cost projection (opt-in; 0 = no cost section)")
	c.Flags().StringVar(&currency, "currency", "USD",
		"currency label for the cost section")
	c.Flags().StringVar(&pricingModel, "pricing-model", "",
		"free-form pricing-model label (e.g. s3-standard) recorded in the cost section")
	c.Flags().StringVar(&format, "format", "json",
		"output format for the report body: json | markdown")
	c.Flags().BoolVar(&skipFleet, "no-fleet", false, "skip the fleet rollup")
	c.Flags().BoolVar(&skipAnomalies, "no-anomalies", false, "skip anomaly detection")
	return c
}

type forecastFlags struct {
	repoURL         string
	deployment      string
	baselineWindow  string
	horizons        []string
	pricePerGBMonth float64
	currency        string
	pricingModel    string
	format          string
	skipFleet       bool
	skipAnomalies   bool
}

func runForecast(cmd *cobra.Command, f forecastFlags) error {
	d := DispatcherFrom(cmd)
	// Positional-or-flag: the URL may arrive via --repo OR the first
	// positional, so we guard the RESOLVED value, not the flag.
	if f.repoURL == "" {
		return missingFlagErr(cmd, "--repo (or the URL positionally)")
	}

	baseline := time.Duration(0)
	if f.baselineWindow != "" {
		dur, err := parseForecastDuration(f.baselineWindow)
		if err != nil {
			return output.NewError("usage.bad_flag",
				fmt.Sprintf("forecast: --baseline-window: %v", err)).Wrap(output.ErrUsage)
		}
		baseline = dur
	}
	var horizons []time.Duration
	for _, h := range f.horizons {
		dur, err := parseForecastDuration(h)
		if err != nil {
			return output.NewError("usage.bad_flag",
				fmt.Sprintf("forecast: --horizon %q: %v", h, err)).Wrap(output.ErrUsage)
		}
		horizons = append(horizons, dur)
	}
	if f.pricePerGBMonth < 0 {
		return output.NewError("usage.bad_flag",
			"forecast: --price-per-gb-month must be >= 0").Wrap(output.ErrUsage)
	}
	switch f.format {
	case "", "json", "markdown":
	default:
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("forecast: --format must be json or markdown; got %q", f.format)).
			Wrap(output.ErrUsage)
	}

	verifier, err := loadVerifier()
	if err != nil {
		return err
	}
	meta, sp, err := openRepo(cmd.Context(), f.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()

	rep, err := forecast.Generate(cmd.Context(), sp, meta, f.repoURL, forecast.Options{
		Verifier:         verifier,
		BaselineWindow:   baseline,
		Horizons:         horizons,
		DeploymentFilter: f.deployment,
		PricePerGBMonth:  f.pricePerGBMonth,
		Currency:         f.currency,
		PricingModel:     f.pricingModel,
		SkipFleet:        f.skipFleet,
		SkipAnomalies:    f.skipAnomalies,
	})
	if err != nil {
		return output.NewError("forecast.failed",
			fmt.Sprintf("forecast: %v", err)).Wrap(err)
	}

	body := forecastReportBody{
		Report: rep,
		format: f.format,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// parseForecastDuration accepts standard time.ParseDuration values
// PLUS the operator-friendly "Nd" form for days. The audit search
// command's parseSinceUntil also accepts duration; this is the
// dual that always returns a duration (not a time).
func parseForecastDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	// Standard parse first (covers "168h", "30m", etc.).
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	// "Nd" → N × 24h.
	if strings.HasSuffix(s, "d") {
		var n int
		if _, err := fmt.Sscanf(s, "%dd", &n); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	return 0, fmt.Errorf("expected duration (e.g. 30d, 90d, 168h); got %q", s)
}

// forecastReportBody wraps the domain Report with renderer hooks
// for both JSON (v1 contract) and Markdown / compact text. JSON
// always emits the underlying Report verbatim via MarshalJSON so
// consumers see only the v1 shape.
type forecastReportBody struct {
	*forecast.Report
	format string
}

// MarshalJSON emits the embedded forecast.Report so the JSON contract stays
// the domain v1 shape.
func (b forecastReportBody) MarshalJSON() ([]byte, error) {
	return stdjson.Marshal(b.Report)
}

// WriteText renders the forecast report to w, choosing the markdown variant
// when format is "markdown" and the compact summary otherwise.
func (b forecastReportBody) WriteText(w io.Writer) error {
	if strings.EqualFold(b.format, "markdown") {
		return forecast.RenderMarkdown(w, b.Report)
	}
	return writeForecastSummary(w, b.Report)
}

// writeForecastSummary renders the compact "single-screen"
// overview for `--format json -o text` (the default + text combo).
// Markdown is reserved for `--format markdown`.
func writeForecastSummary(w io.Writer, r *forecast.Report) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "forecast — %s\n", r.URL)
	fmt.Fprintf(bw, "  Baseline:    %s window (%s → %s)\n",
		(time.Duration(r.BaselineWindowSeconds) * time.Second).String(),
		r.BaselineSince.Format(time.RFC3339),
		r.BaselineUntil.Format(time.RFC3339))
	fmt.Fprintf(bw, "  Horizons:    %s\n",
		strings.Join(horizonNamesFromSeconds(r.HorizonsSeconds), ", "))
	if r.DeploymentFilter != "" {
		fmt.Fprintf(bw, "  Filter:      deployment %q\n", r.DeploymentFilter)
	}
	fmt.Fprintf(bw, "  Walk:        %d ms\n", r.DurationMS)
	fmt.Fprintln(bw)

	if r.Fleet != nil {
		fmt.Fprintf(bw, "Fleet now:                %s\n", humanBytes(r.Fleet.TotalCurrentBytes))
		for _, p := range r.Fleet.TotalProjections {
			fmt.Fprintf(bw, "Fleet at %-5s (%s): %s\n",
				p.HorizonName, p.AtDate.Format("2006-01-02"),
				humanBytes(p.ProjectedBytes))
		}
		fmt.Fprintln(bw)
	}

	if r.Cost != nil {
		fmt.Fprintf(bw, "Current monthly cost:     %s %.2f at %s %.4f/GB-month (%s)\n",
			r.Cost.Currency, r.Cost.CurrentMonthly,
			r.Cost.Currency, r.Cost.PricePerGBMonth,
			fallback(r.Cost.PricingModel, "unspecified"))
		for _, p := range r.Cost.Projections {
			fmt.Fprintf(bw, "Projected monthly (%s):  %s %.2f\n",
				p.HorizonName, r.Cost.Currency, p.ProjectedMonthlyCost)
		}
		fmt.Fprintln(bw)
	}

	if len(r.Deployments) == 0 {
		fmt.Fprintln(bw, "(no deployments)")
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	fmt.Fprintln(bw, "Per-deployment:")
	for _, d := range r.Deployments {
		fmt.Fprintf(bw, "  %-20s now=%s  rate=%s  R²=%.2f  conf=%s\n",
			d.Name, humanBytes(d.CurrentBytes),
			humanRateCompact(d.BytesPerDay), d.RSquared, d.Confidence)
		for _, p := range d.Projections {
			fmt.Fprintf(bw, "      %-5s → %s\n",
				p.HorizonName, humanBytes(p.ProjectedBytes))
		}
	}
	if len(r.Anomalies) > 0 {
		fmt.Fprintln(bw)
		fmt.Fprintf(bw, "✗ %d growth anomal", len(r.Anomalies))
		if len(r.Anomalies) == 1 {
			fmt.Fprintln(bw, "y detected:")
		} else {
			fmt.Fprintln(bw, "ies detected:")
		}
		for _, a := range r.Anomalies {
			fmt.Fprintf(bw, "    %s — %s (%.2fx baseline)\n",
				a.Deployment, a.Reason, a.MultiplierObserved)
		}
	}
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "Use --format markdown for the full report (per-deployment tables, methodology notes).")
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

func humanRateCompact(bytesPerDay float64) string {
	const unit = 1024.0
	if bytesPerDay < unit {
		return fmt.Sprintf("%.0fB/d", bytesPerDay)
	}
	div, exp := unit, 0
	for n := bytesPerDay / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB/d", bytesPerDay/div, "KMGTPE"[exp])
}

// horizonNamesFromSeconds is the CLI-side mirror of the same name
// in internal/forecast (helper for the compact summary). Duplicated
// here to avoid exposing it from the domain package.
func horizonNamesFromSeconds(secs []int64) []string {
	out := make([]string, 0, len(secs))
	for _, s := range secs {
		d := time.Duration(s) * time.Second
		days := int64(d.Hours()) / 24
		if days > 0 {
			out = append(out, fmt.Sprintf("%dd", days))
		} else {
			out = append(out, fmt.Sprintf("%dh", int64(d.Hours())))
		}
	}
	return out
}

func fallback(s, dflt string) string {
	if s == "" {
		return dflt
	}
	return s
}
