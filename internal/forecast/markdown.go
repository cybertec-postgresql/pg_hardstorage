// markdown.go — forensics-grade Markdown renderer for capacity/growth forecast reports.
package forecast

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// RenderMarkdown writes the forecast as a forensics-grade Markdown
// document. Layout matches the compliance report's discipline:
// top-of-page metadata table, fixed-order H2 sections, GFM tables
// for everything tabular.
func RenderMarkdown(w io.Writer, r *Report) error {
	if r == nil {
		return fmt.Errorf("forecast: nil Report")
	}
	bw := &strings.Builder{}
	writeHeader(bw, r)
	writeFleetSection(bw, r)
	writeDeploymentSection(bw, r)
	writeCostSection(bw, r)
	writeAnomalySection(bw, r)
	writeNotesSection(bw, r)
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n")+"\n")
	return err
}

func writeHeader(bw *strings.Builder, r *Report) {
	fmt.Fprintln(bw, "# pg_hardstorage forecast report")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "| Field | Value |")
	fmt.Fprintln(bw, "| --- | --- |")
	fmt.Fprintf(bw, "| Repository | `%s` |\n", r.URL)
	if r.Repo != nil {
		if r.Repo.ID != "" {
			fmt.Fprintf(bw, "| Repository ID | `%s` |\n", r.Repo.ID)
		}
		if r.Repo.Mode != "" {
			fmt.Fprintf(bw, "| Mode | %s |\n", r.Repo.Mode)
		}
	}
	fmt.Fprintf(bw, "| Baseline window | %s |\n",
		(time.Duration(r.BaselineWindowSeconds) * time.Second).String())
	fmt.Fprintf(bw, "| Baseline since | %s |\n", r.BaselineSince.Format(time.RFC3339))
	fmt.Fprintf(bw, "| Baseline until | %s |\n", r.BaselineUntil.Format(time.RFC3339))
	fmt.Fprintf(bw, "| Horizons | %s |\n", strings.Join(horizonNamesFromSeconds(r.HorizonsSeconds), ", "))
	if r.DeploymentFilter != "" {
		fmt.Fprintf(bw, "| Filter | deployment `%s` |\n", r.DeploymentFilter)
	}
	fmt.Fprintf(bw, "| Generated at | %s |\n", r.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "| Walk duration | %d ms |\n", r.DurationMS)
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "_Linear regression on observed manifest commit history. Confidence per deployment reflects sample count + R² of the fit; sparse-data deployments project flat. The forecast is a planning tool, not a guarantee._")
	fmt.Fprintln(bw)
}

func writeFleetSection(bw *strings.Builder, r *Report) {
	fmt.Fprintln(bw, "## Fleet projection")
	fmt.Fprintln(bw)
	if r.Fleet == nil {
		fmt.Fprintln(bw, "(skipped)")
		fmt.Fprintln(bw)
		return
	}
	fmt.Fprintf(bw, "**Current fleet size:** %s.\n\n", humanBytes(r.Fleet.TotalCurrentBytes))
	if len(r.Fleet.TotalProjections) == 0 {
		fmt.Fprintln(bw, "No projections (no horizons configured).")
		fmt.Fprintln(bw)
		return
	}
	fmt.Fprintln(bw, "| Horizon | At date | Projected fleet size |")
	fmt.Fprintln(bw, "| --- | --- | --- |")
	for _, p := range r.Fleet.TotalProjections {
		fmt.Fprintf(bw, "| %s | %s | %s |\n",
			p.HorizonName,
			p.AtDate.Format(time.RFC3339),
			humanBytes(p.ProjectedBytes))
	}
	fmt.Fprintln(bw)
}

func writeDeploymentSection(bw *strings.Builder, r *Report) {
	fmt.Fprintln(bw, "## Per-deployment forecast")
	fmt.Fprintln(bw)
	if len(r.Deployments) == 0 {
		fmt.Fprintln(bw, "(no deployments)")
		fmt.Fprintln(bw)
		return
	}
	for _, d := range r.Deployments {
		fmt.Fprintf(bw, "### `%s`\n\n", d.Name)
		fmt.Fprintln(bw, "| Field | Value |")
		fmt.Fprintln(bw, "| --- | --- |")
		fmt.Fprintf(bw, "| Current size | %s |\n", humanBytes(d.CurrentBytes))
		fmt.Fprintf(bw, "| Manifest count | %d |\n", d.CurrentManifests)
		if !d.LatestStoppedAt.IsZero() {
			fmt.Fprintf(bw, "| Newest backup | %s |\n", d.LatestStoppedAt.Format(time.RFC3339))
		}
		if !d.OldestStoppedAt.IsZero() {
			fmt.Fprintf(bw, "| Oldest backup | %s |\n", d.OldestStoppedAt.Format(time.RFC3339))
		}
		fmt.Fprintf(bw, "| Samples observed | %d |\n", d.SamplesObserved)
		fmt.Fprintf(bw, "| Confidence | %s |\n", confidenceMarkdown(d.Confidence))
		if d.SamplesObserved >= MinSamples {
			fmt.Fprintf(bw, "| Bytes/day | %s |\n", humanBytesPerDay(d.BytesPerDay))
			fmt.Fprintf(bw, "| Manifests/day | %.2f |\n", d.ManifestsPerDay)
			fmt.Fprintf(bw, "| R² | %.3f |\n", d.RSquared)
		}
		if d.Note != "" {
			fmt.Fprintf(bw, "| Note | %s |\n", d.Note)
		}
		fmt.Fprintln(bw)
		if len(d.Projections) > 0 {
			fmt.Fprintln(bw, "**Projections:**")
			fmt.Fprintln(bw)
			fmt.Fprintln(bw, "| Horizon | At date | Projected size | Manifests |")
			fmt.Fprintln(bw, "| --- | --- | --- | --- |")
			for _, p := range d.Projections {
				fmt.Fprintf(bw, "| %s | %s | %s | %d |\n",
					p.HorizonName,
					p.AtDate.Format(time.RFC3339),
					humanBytes(p.ProjectedBytes),
					p.ProjectedManifests)
			}
			fmt.Fprintln(bw)
		}
	}
}

func writeCostSection(bw *strings.Builder, r *Report) {
	fmt.Fprintln(bw, "## Cost projection")
	fmt.Fprintln(bw)
	if r.Cost == nil {
		fmt.Fprintln(bw, "_No cost projection (operator did not supply --price-per-gb-month). Pass a rate to monetise the projection._")
		fmt.Fprintln(bw)
		return
	}
	model := r.Cost.PricingModel
	if model == "" {
		model = "(unspecified)"
	}
	fmt.Fprintf(bw, "**Current monthly cost:** %s %.2f at %s %.4f/GB-month (model: %s).\n\n",
		r.Cost.Currency, r.Cost.CurrentMonthly,
		r.Cost.Currency, r.Cost.PricePerGBMonth, model)
	fmt.Fprintln(bw, "| Horizon | At date | Projected size | Projected monthly cost |")
	fmt.Fprintln(bw, "| --- | --- | --- | --- |")
	for _, p := range r.Cost.Projections {
		fmt.Fprintf(bw, "| %s | %s | %s | %s %.2f |\n",
			p.HorizonName,
			p.AtDate.Format(time.RFC3339),
			humanBytes(p.ProjectedBytes),
			r.Cost.Currency, p.ProjectedMonthlyCost)
	}
	fmt.Fprintln(bw)
}

func writeAnomalySection(bw *strings.Builder, r *Report) {
	fmt.Fprintln(bw, "## Growth anomalies")
	fmt.Fprintln(bw)
	if len(r.Anomalies) == 0 {
		fmt.Fprintln(bw, "✓ No deployments show growth-rate shifts beyond the configured threshold.")
		fmt.Fprintln(bw)
		return
	}
	fmt.Fprintln(bw, "| Deployment | Reason | Baseline rate | Recent rate | Multiplier |")
	fmt.Fprintln(bw, "| --- | --- | --- | --- | --- |")
	for _, a := range r.Anomalies {
		fmt.Fprintf(bw, "| `%s` | %s | %s | %s | %.2fx |\n",
			a.Deployment, a.Reason,
			humanBytesPerDay(a.BaselineBytesPerDay),
			humanBytesPerDay(a.RecentBytesPerDay),
			a.MultiplierObserved)
	}
	fmt.Fprintln(bw)
	fmt.Fprintf(bw, "_Anomaly threshold: %.1f×._\n\n", AnomalyMultiplier)
}

func writeNotesSection(bw *strings.Builder, r *Report) {
	fmt.Fprintln(bw, "## Methodology notes")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "- Sizes are **logical bytes** (sum of `FileEntry.Size` from each manifest). Logical = on-source data size, before dedup / compression / encryption. Operators reason about repo growth in terms of \"what your DB looks like\", not what bytes ended up on S3.")
	fmt.Fprintf(bw, "- Growth rate is fitted by ordinary least-squares linear regression on the per-day cumulative-bytes series. R² = goodness of fit; we surface it raw so operators can apply their own quality threshold.\n")
	fmt.Fprintf(bw, "- Confidence: **insufficient** if fewer than %d manifests in the baseline window; **low** otherwise unless R² ≥ 0.5 (medium) or R² ≥ 0.85 with ≥ 5 samples (high).\n", MinSamples)
	fmt.Fprintf(bw, "- Anomaly detection compares the rate over the last %s to the rate over the rest of the baseline window; a multiplier ≥ %.1f surfaces a `sudden_uptick` finding.\n",
		AnomalyTailWindow, AnomalyMultiplier)
	fmt.Fprintln(bw, "- A negative regression slope is clamped to 0 — a backup repo's logical bytes don't shrink in any operationally meaningful way; that signal would be from manifests being deleted, which is a different report.")
	fmt.Fprintln(bw, "- Cost projection is a strictly linear conversion at the operator-supplied price; cloud tariffs include request fees + egress + lifecycle storage classes that the operator's billing system handles, not us.")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "---")
	fmt.Fprintln(bw)
	fmt.Fprintf(bw, "_Schema `%s`. Walk duration %d ms._\n", r.Schema, r.DurationMS)
}

// confidenceMarkdown decorates the textual confidence value with a
// glyph for fast visual scanning.
func confidenceMarkdown(c string) string {
	switch c {
	case "high":
		return "✓ high"
	case "medium":
		return "· medium"
	case "low":
		return "⚠ low"
	case "insufficient":
		return "⚠ insufficient (sparse data)"
	default:
		return c
	}
}

// horizonNamesFromSeconds re-derives the H labels from the
// duration list (avoids re-storing them in the report).
func horizonNamesFromSeconds(secs []int64) []string {
	out := make([]string, 0, len(secs))
	cp := append([]int64{}, secs...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	for _, s := range cp {
		d := time.Duration(s) * time.Second
		out = append(out, humanHorizon(d))
	}
	return out
}

// humanBytes renders a byte count as a 4-significant-figure
// human-readable form (KB / MB / GB / TB / PB). Localised to the
// 1024-unit convention the rest of the binary uses.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	suffix := "KMGTPE"[exp]
	return fmt.Sprintf("%.2f %ciB", float64(b)/float64(div), suffix)
}

// humanBytesPerDay is humanBytes + "/day" suffix. Kept as a helper
// because the rate fields are common in the Markdown.
func humanBytesPerDay(rate float64) string {
	const unit = 1024.0
	if rate < unit {
		return fmt.Sprintf("%.0f B/day", rate)
	}
	div, exp := unit, 0
	for n := rate / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	suffix := "KMGTPE"[exp]
	return fmt.Sprintf("%.2f %ciB/day", rate/div, suffix)
}
