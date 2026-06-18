// Package forecast produces capacity-planning + cost-projection
// reports behind `pg_hardstorage forecast`.
//
// The operator question this answers: "given how the fleet has
// grown over the last N days, where will we be in 30/90/365 days,
// and what will it cost?". Different from the other diagnostic
// surfaces:
//
//   - doctor       — present-state health (host / config / connectivity)
//   - repo audit   — present-state inventory (counts / breakdowns)
//   - compliance   — historical events (audit-log rollup over a window)
//   - forecast     — historical trends → forward projections
//
// Design discipline:
//
//   - Linear regression on observable points (manifest StoppedAt +
//     logical bytes) produces a slope (bytes/day, manifests/day) +
//     R² for confidence reporting. We never claim more than the
//     data supports — sparse-data deployments get "low confidence"
//     and a note explaining why.
//
//   - Read-only by construction: walks manifests + audit log, never
//     mutates anything. Safe at any cadence including against
//     WORM-locked repos.
//
//   - Cost projection is OPT-IN via --price-per-gb-month. We don't
//     try to look up cloud pricing automatically (it changes; it's
//     account-specific; tariff arithmetic is the user's job). When
//     the operator passes a rate we project the obvious linear
//     cost; that's the reasonable single-number answer.
//
//   - Anomaly detection compares the most-recent 7-day rate to the
//     baseline-window-without-the-tail rate; a >2× shift surfaces
//     as a structured anomaly. This is the same "watch for sudden
//     uptick" signal an SRE would manually compute.
//
// Read-only by construction; safe at any cadence including
// against a WORM-locked repo.
package forecast

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// ReportSchema is the on-disk version tag for Report bodies. Stable
// per the v1 schema commitment.
const ReportSchema = "pg_hardstorage.forecast.v1"

// DefaultBaselineWindow is the historical window we observe to fit
// growth rates. 90 days is a reasonable trade-off between
// "enough data for stable regression" and "recent-enough that the
// trend is still relevant".
const DefaultBaselineWindow = 90 * 24 * time.Hour

// AnomalyTailWindow is the recent slice we compare to the baseline
// when looking for sudden shifts. 7 days catches "this week's
// growth is 5× normal" without being so short that noise dominates.
const AnomalyTailWindow = 7 * 24 * time.Hour

// AnomalyMultiplier is the threshold for "sudden uptick" detection.
// A tail-window growth rate that's > AnomalyMultiplier × the
// baseline-without-tail rate fires the anomaly signal. 2× is
// conservative — daily noise can hit 1.5×, but sustained 2× over a
// week is signal.
const AnomalyMultiplier = 2.0

// DefaultHorizons is the standard projection set: 30 days
// (operational planning), 90 days (quarterly budget), 365 days
// (annual capacity / contract negotiations).
var DefaultHorizons = []time.Duration{
	30 * 24 * time.Hour,
	90 * 24 * time.Hour,
	365 * 24 * time.Hour,
}

// Report is the structured forecast body. Every field is JSON-stable
// per the v1 schema commitment.
type Report struct {
	Schema      string    `json:"schema"`
	GeneratedAt time.Time `json:"generated_at"`
	StoppedAt   time.Time `json:"stopped_at"`
	DurationMS  int64     `json:"duration_ms"`

	URL  string       `json:"url"`
	Repo *RepoSummary `json:"repo,omitempty"`

	BaselineWindowSeconds int64     `json:"baseline_window_seconds"`
	BaselineSince         time.Time `json:"baseline_since"`
	BaselineUntil         time.Time `json:"baseline_until"`

	HorizonsSeconds []int64 `json:"horizons_seconds"`

	DeploymentFilter string `json:"deployment_filter,omitempty"`

	// Per-deployment forecasts. Sorted by deployment name for
	// stable JSON.
	Deployments []DeploymentForecast `json:"deployments"`

	// Fleet-wide rollup. Only the totals (current + projected); per-
	// deployment breakdowns live in the slice above.
	Fleet *FleetForecast `json:"fleet,omitempty"`

	// Cost projection. Only populated when the operator supplies a
	// price per GB-month via Options.PricePerGBMonth.
	Cost *CostForecast `json:"cost,omitempty"`

	// Detected growth anomalies. Empty when nothing crosses the
	// AnomalyMultiplier threshold.
	Anomalies []GrowthAnomaly `json:"anomalies,omitempty"`
}

// RepoSummary mirrors the static metadata from HSREPO. Same shape
// as repoaudit / compliance — operators reading multiple reports
// see consistent header data.
type RepoSummary struct {
	ID        string `json:"id,omitempty"`
	Schema    string `json:"schema,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Mode      string `json:"mode,omitempty"`
}

// DeploymentForecast describes one deployment's growth picture.
type DeploymentForecast struct {
	Name string `json:"name"`

	// Current state — the freshest backup we observed.
	CurrentBytes     int64     `json:"current_bytes"`
	CurrentManifests int       `json:"current_manifests"`
	LatestStoppedAt  time.Time `json:"latest_stopped_at,omitempty"`
	OldestStoppedAt  time.Time `json:"oldest_stopped_at,omitempty"`

	// SamplesObserved is the number of manifests in the baseline
	// window. Below MinSamples (3) we can't fit a meaningful line.
	SamplesObserved int `json:"samples_observed"`

	// Growth rates derived from regression. Zero on insufficient
	// data; Confidence reports the quality.
	BytesPerDay     float64 `json:"bytes_per_day,omitempty"`
	ManifestsPerDay float64 `json:"manifests_per_day,omitempty"`

	// RSquared is the coefficient of determination on the bytes
	// regression. 1.0 = perfect fit; 0.0 = no linear relationship.
	// We surface it raw so the operator can apply their own
	// quality threshold.
	RSquared float64 `json:"r_squared,omitempty"`

	// Confidence is a coarse "high / medium / low / insufficient"
	// classification suitable for the Markdown renderer.
	Confidence string `json:"confidence"`

	// DedupRatio is the average ratio across observed manifests
	// where it's computable (1.0 means no dedup; > 1.0 means N
	// logical bytes for 1 physical byte). Empty (0) when no
	// manifests in window have decompressed dedup info.
	DedupRatio float64 `json:"dedup_ratio,omitempty"`

	// Note carries a human-readable caveat (e.g. "fewer than
	// MinSamples observations; projection is the latest size with
	// zero growth").
	Note string `json:"note,omitempty"`

	// Per-horizon projections. Always populated even when
	// Confidence == "insufficient" — the projection in that case
	// is "current bytes, no growth".
	Projections []Projection `json:"projections"`
}

// Projection is one horizon's prediction for a deployment.
type Projection struct {
	HorizonName        string    `json:"horizon_name"` // "30d", "90d", "365d", or "Nh"
	HorizonSeconds     int64     `json:"horizon_seconds"`
	AtDate             time.Time `json:"at_date"`
	ProjectedBytes     int64     `json:"projected_bytes"`
	ProjectedManifests int       `json:"projected_manifests"`
}

// FleetForecast is the fleet-wide rollup.
type FleetForecast struct {
	TotalCurrentBytes int64             `json:"total_current_bytes"`
	TotalProjections  []FleetProjection `json:"total_projections"`
}

// FleetProjection is one horizon's fleet-total prediction.
type FleetProjection struct {
	HorizonName    string    `json:"horizon_name"`
	HorizonSeconds int64     `json:"horizon_seconds"`
	AtDate         time.Time `json:"at_date"`
	ProjectedBytes int64     `json:"projected_bytes"`
}

// CostForecast monetises the fleet projection at a configurable
// rate. Currency is "USD" by default; the operator picks. Pricing
// model is a free-form label so an operator who's running on
// "S3 Standard-IA" or "GCS Coldline" can record which tariff the
// projection assumes.
type CostForecast struct {
	PricePerGBMonth float64          `json:"price_per_gb_month"`
	Currency        string           `json:"currency"`
	PricingModel    string           `json:"pricing_model,omitempty"`
	CurrentMonthly  float64          `json:"current_monthly_cost"`
	Projections     []CostProjection `json:"projections"`
}

// CostProjection is one horizon's cost prediction.
type CostProjection struct {
	HorizonName          string    `json:"horizon_name"`
	HorizonSeconds       int64     `json:"horizon_seconds"`
	AtDate               time.Time `json:"at_date"`
	ProjectedBytes       int64     `json:"projected_bytes"`
	ProjectedMonthlyCost float64   `json:"projected_monthly_cost"`
}

// GrowthAnomaly records a deployment whose recent rate diverged
// significantly from the baseline. The structured record is what
// alerting / Slack / dashboard tools consume.
type GrowthAnomaly struct {
	Deployment          string  `json:"deployment"`
	Reason              string  `json:"reason"` // "sudden_uptick" | "sudden_drop"
	BaselineBytesPerDay float64 `json:"baseline_bytes_per_day"`
	RecentBytesPerDay   float64 `json:"recent_bytes_per_day"`
	MultiplierObserved  float64 `json:"multiplier_observed"`
	MultiplierThreshold float64 `json:"multiplier_threshold"`
}

// Options configures one forecast run.
type Options struct {
	// Verifier validates each manifest's signature at iteration
	// time. Required.
	Verifier *backup.Verifier

	// Now is the reference time for the report. Default
	// time.Now().UTC(). Tests pin it for determinism.
	Now time.Time

	// BaselineWindow is the historical window over which we fit
	// growth rates. Default DefaultBaselineWindow (90d).
	BaselineWindow time.Duration

	// Horizons are the future durations to project to. Default
	// DefaultHorizons.
	Horizons []time.Duration

	// DeploymentFilter restricts the report to one deployment.
	DeploymentFilter string

	// PricePerGBMonth, when > 0, populates the CostForecast section.
	PricePerGBMonth float64

	// Currency labels the cost section. Default "USD".
	Currency string

	// PricingModel labels the cost section ("s3-standard",
	// "gcs-standard", etc.). Free-form — we don't validate.
	PricingModel string

	// SkipAnomalies suppresses the anomaly-detection pass (the
	// only meaningful reason: a deployment with very sparse data
	// makes the tail-vs-baseline comparison noisy; operators
	// running quarterly reports may want a clean report without
	// the noise).
	SkipAnomalies bool

	// SkipFleet suppresses the fleet-wide rollup. Useful when
	// running per-deployment forecasts to compare against each
	// other without the fleet line.
	SkipFleet bool
}

// MinSamples is the minimum number of manifests we need in the
// baseline window before fitting a regression. Below this, the
// deployment gets a "insufficient" Confidence and its projection
// is "current bytes, zero growth".
const MinSamples = 3

// Generate runs one forecast and returns the typed Report.
func Generate(ctx context.Context, sp storage.StoragePlugin, meta *repo.Metadata, repoURL string, opts Options) (*Report, error) {
	if sp == nil {
		return nil, errors.New("forecast: nil StoragePlugin")
	}
	if opts.Verifier == nil {
		return nil, errors.New("forecast: Verifier is required")
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	if opts.BaselineWindow <= 0 {
		opts.BaselineWindow = DefaultBaselineWindow
	}
	if len(opts.Horizons) == 0 {
		opts.Horizons = DefaultHorizons
	}
	if opts.Currency == "" {
		opts.Currency = "USD"
	}

	started := time.Now().UTC()
	r := &Report{
		Schema:                ReportSchema,
		GeneratedAt:           opts.Now.UTC(),
		URL:                   repoURL,
		BaselineWindowSeconds: int64(opts.BaselineWindow / time.Second),
		BaselineSince:         opts.Now.Add(-opts.BaselineWindow).UTC(),
		BaselineUntil:         opts.Now.UTC(),
		DeploymentFilter:      opts.DeploymentFilter,
	}
	for _, h := range opts.Horizons {
		r.HorizonsSeconds = append(r.HorizonsSeconds, int64(h/time.Second))
	}
	finish := func() {
		r.StoppedAt = time.Now().UTC()
		r.DurationMS = r.StoppedAt.Sub(started).Milliseconds()
	}

	if meta != nil {
		r.Repo = &RepoSummary{
			ID:        meta.ID,
			Schema:    meta.Schema,
			CreatedAt: meta.CreatedAt,
			Mode:      string(meta.Mode),
		}
	}

	store := backup.NewManifestStore(sp)
	deployments, err := store.Deployments(ctx)
	if err != nil {
		finish()
		return r, fmt.Errorf("forecast: enumerate deployments: %w", err)
	}
	sort.Strings(deployments)

	// Walk per-deployment, collecting samples.
	for _, dep := range deployments {
		if opts.DeploymentFilter != "" && opts.DeploymentFilter != dep {
			continue
		}
		if err := ctx.Err(); err != nil {
			finish()
			return r, err
		}
		f := buildDeploymentForecast(ctx, store, dep, opts)
		r.Deployments = append(r.Deployments, f)
	}

	if !opts.SkipFleet {
		r.Fleet = buildFleet(r.Deployments, opts)
	}
	if opts.PricePerGBMonth > 0 {
		r.Cost = buildCost(r, opts)
	}
	if !opts.SkipAnomalies {
		r.Anomalies = detectAnomalies(ctx, store, r.Deployments, opts)
	}

	finish()
	return r, nil
}

// sample is one observation: a manifest's StoppedAt + logical bytes.
type sample struct {
	at    time.Time
	bytes int64
}

// buildDeploymentForecast walks one deployment's manifests and
// computes the growth picture. All errors here are swallowed (the
// forecast is best-effort by design — a partial fleet projection is
// more useful than a hard failure).
func buildDeploymentForecast(ctx context.Context, store *backup.ManifestStore, dep string, opts Options) DeploymentForecast {
	out := DeploymentForecast{Name: dep, Projections: zeroProjections(opts)}

	cumulativeByDay := map[int64]int64{} // unix-day → cumulative bytes
	var allSamples []sample
	var inWindow []sample

	manifestCount := 0
	var current int64
	var latest, oldest time.Time

	for m, lerr := range store.List(ctx, dep, opts.Verifier) {
		if lerr != nil {
			continue
		}
		manifestCount++
		size := manifestLogicalBytes(m)

		// Unconditional accumulators.
		if oldest.IsZero() || m.StoppedAt.Before(oldest) {
			oldest = m.StoppedAt
		}
		if m.StoppedAt.After(latest) {
			latest = m.StoppedAt
			current = size
		}
		allSamples = append(allSamples, sample{at: m.StoppedAt, bytes: size})

		// Window-bounded accumulator. The growth-rate fit only
		// sees backups in [Now - BaselineWindow, Now].
		if !m.StoppedAt.Before(opts.Now.Add(-opts.BaselineWindow)) &&
			!m.StoppedAt.After(opts.Now) {
			inWindow = append(inWindow, sample{at: m.StoppedAt, bytes: size})
			day := m.StoppedAt.UTC().Truncate(24 * time.Hour).Unix()
			// Cumulative bytes up to and including this manifest's
			// day. We track per-day cumulative to feed the
			// regression on a daily series; a deployment with N
			// backups per day still contributes N to the
			// manifest count but the size series is daily-totaled.
			cumulativeByDay[day] += size
		}
	}

	out.CurrentManifests = manifestCount
	out.CurrentBytes = current
	out.LatestStoppedAt = latest
	out.OldestStoppedAt = oldest
	out.SamplesObserved = len(inWindow)

	// Build the daily cumulative series for regression. Sort by day,
	// running-sum the daily totals.
	type dailyPoint struct {
		day        int64
		cumulative int64
	}
	days := make([]int64, 0, len(cumulativeByDay))
	for d := range cumulativeByDay {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i] < days[j] })

	var series []dailyPoint
	var running int64
	for _, d := range days {
		running += cumulativeByDay[d]
		series = append(series, dailyPoint{day: d, cumulative: running})
	}

	// Regression and confidence.
	if len(inWindow) < MinSamples {
		out.Confidence = "insufficient"
		out.Note = fmt.Sprintf("fewer than %d manifests in baseline window; projection assumes zero growth from current size",
			MinSamples)
		out.Projections = projectFlat(out.CurrentBytes, manifestCount, opts)
		return out
	}

	// Convert series to (x=days-since-baseline-start, y=cumulative-bytes)
	// for the linear fit. x in days keeps the slope in bytes/day.
	bxs := make([]float64, len(series))
	bys := make([]float64, len(series))
	startDay := series[0].day
	for i, p := range series {
		bxs[i] = float64(p.day - startDay)
		bys[i] = float64(p.cumulative)
	}
	slope, _, r2 := linearRegress(bxs, bys)

	// manifests per day = manifest count / window-days-spanned
	manifestRate := manifestsPerDay(inWindow, opts.Now)

	out.BytesPerDay = math.Max(0, slope) // negative slope is meaningless for a backup repo
	out.ManifestsPerDay = manifestRate
	out.RSquared = r2

	switch {
	case r2 >= 0.85 && len(inWindow) >= 5:
		out.Confidence = "high"
	case r2 >= 0.5 && len(inWindow) >= 3:
		out.Confidence = "medium"
	default:
		out.Confidence = "low"
	}

	// Build per-horizon projections from the rate.
	out.Projections = make([]Projection, 0, len(opts.Horizons))
	for _, h := range opts.Horizons {
		days := h.Hours() / 24
		out.Projections = append(out.Projections, Projection{
			HorizonName:        humanHorizon(h),
			HorizonSeconds:     int64(h / time.Second),
			AtDate:             opts.Now.Add(h).UTC(),
			ProjectedBytes:     out.CurrentBytes + int64(out.BytesPerDay*days),
			ProjectedManifests: manifestCount + int(math.Round(out.ManifestsPerDay*days)),
		})
	}
	return out
}

// projectFlat builds the "no growth" projection set for deployments
// with insufficient data.
func projectFlat(currentBytes int64, manifests int, opts Options) []Projection {
	out := make([]Projection, 0, len(opts.Horizons))
	for _, h := range opts.Horizons {
		out = append(out, Projection{
			HorizonName:        humanHorizon(h),
			HorizonSeconds:     int64(h / time.Second),
			AtDate:             opts.Now.Add(h).UTC(),
			ProjectedBytes:     currentBytes,
			ProjectedManifests: manifests,
		})
	}
	return out
}

// zeroProjections is the "no data at all" stub. Used as the
// initial value before sample collection so absent-deployment
// rows (no manifests at all) still serialise cleanly.
func zeroProjections(opts Options) []Projection {
	out := make([]Projection, 0, len(opts.Horizons))
	for _, h := range opts.Horizons {
		out = append(out, Projection{
			HorizonName:    humanHorizon(h),
			HorizonSeconds: int64(h / time.Second),
			AtDate:         opts.Now.Add(h).UTC(),
		})
	}
	return out
}

// manifestsPerDay computes the manifest-commit rate over the
// observed in-window timestamps. We don't fit a regression here
// — backup count is integer-valued and the operator question is
// "how often do backups happen?". Rate = N / (max-min span in
// days, floored at 1).
func manifestsPerDay(in []sample, now time.Time) float64 {
	if len(in) == 0 {
		return 0
	}
	var first, last time.Time
	for _, s := range in {
		if first.IsZero() || s.at.Before(first) {
			first = s.at
		}
		if s.at.After(last) {
			last = s.at
		}
	}
	span := last.Sub(first).Hours() / 24
	if span < 1 {
		span = 1 // single-day samples → 1-day denominator
	}
	return float64(len(in)) / span
}

// linearRegress fits y = a + b*x by ordinary least squares. Returns
// (slope, intercept, r-squared). Pure function; deterministic.
func linearRegress(x, y []float64) (slope, intercept, r2 float64) {
	n := len(x)
	if n < 2 || len(y) != n {
		return 0, 0, 0
	}
	var sumX, sumY, sumXY, sumX2 float64
	for i := 0; i < n; i++ {
		sumX += x[i]
		sumY += y[i]
		sumXY += x[i] * y[i]
		sumX2 += x[i] * x[i]
	}
	meanX := sumX / float64(n)
	meanY := sumY / float64(n)
	denom := sumX2 - float64(n)*meanX*meanX
	if denom == 0 {
		// All x's identical → vertical fit is meaningless.
		return 0, meanY, 0
	}
	slope = (sumXY - float64(n)*meanX*meanY) / denom
	intercept = meanY - slope*meanX
	// R²: 1 - SSres/SStot
	var ssRes, ssTot float64
	for i := 0; i < n; i++ {
		pred := intercept + slope*x[i]
		ssRes += (y[i] - pred) * (y[i] - pred)
		ssTot += (y[i] - meanY) * (y[i] - meanY)
	}
	if ssTot == 0 {
		// All y's identical → perfect "no growth" fit. Report 1.0
		// so confidence reads "high" rather than "no relationship";
		// the slope is zero anyway.
		return slope, intercept, 1.0
	}
	r2 = 1 - ssRes/ssTot
	if r2 < 0 {
		r2 = 0 // negative R² (worse than mean) clamps to 0
	}
	return slope, intercept, r2
}

// manifestLogicalBytes sums the FileEntry sizes. This is the
// backup's "logical bytes" — not de-duplicated, not encrypted,
// not compressed; the on-source size of the data. Operators
// reason about repo growth in terms of "what your DB looks like",
// not "what bytes ended up on S3 after dedup".
func manifestLogicalBytes(m *backup.Manifest) int64 {
	var total int64
	for _, f := range m.Files {
		total += f.Size
	}
	return total
}

// humanHorizon turns a duration into a compact label. Days for
// multi-day; hours otherwise.
func humanHorizon(h time.Duration) string {
	d := int64(h.Hours()) / 24
	if d > 0 {
		return fmt.Sprintf("%dd", d)
	}
	return fmt.Sprintf("%dh", int64(h.Hours()))
}

// buildFleet sums per-deployment projections into the fleet rollup.
func buildFleet(rows []DeploymentForecast, opts Options) *FleetForecast {
	out := &FleetForecast{}
	byHorizon := map[string]*FleetProjection{}
	for _, r := range rows {
		out.TotalCurrentBytes += r.CurrentBytes
		for _, p := range r.Projections {
			fp := byHorizon[p.HorizonName]
			if fp == nil {
				fp = &FleetProjection{
					HorizonName:    p.HorizonName,
					HorizonSeconds: p.HorizonSeconds,
					AtDate:         p.AtDate,
				}
				byHorizon[p.HorizonName] = fp
			}
			fp.ProjectedBytes += p.ProjectedBytes
		}
	}
	// Materialise in horizon-ascending order matching opts.Horizons.
	for _, h := range opts.Horizons {
		name := humanHorizon(h)
		if fp, ok := byHorizon[name]; ok {
			out.TotalProjections = append(out.TotalProjections, *fp)
		}
	}
	return out
}

// buildCost monetises the fleet projection at a fixed rate.
func buildCost(r *Report, opts Options) *CostForecast {
	if r.Fleet == nil {
		return nil
	}
	out := &CostForecast{
		PricePerGBMonth: opts.PricePerGBMonth,
		Currency:        opts.Currency,
		PricingModel:    opts.PricingModel,
		CurrentMonthly:  bytesToMonthlyCost(r.Fleet.TotalCurrentBytes, opts.PricePerGBMonth),
	}
	for _, p := range r.Fleet.TotalProjections {
		out.Projections = append(out.Projections, CostProjection{
			HorizonName:          p.HorizonName,
			HorizonSeconds:       p.HorizonSeconds,
			AtDate:               p.AtDate,
			ProjectedBytes:       p.ProjectedBytes,
			ProjectedMonthlyCost: bytesToMonthlyCost(p.ProjectedBytes, opts.PricePerGBMonth),
		})
	}
	return out
}

// bytesToMonthlyCost is the rounded-to-cents linear conversion. A
// GB here is 1 GiB (1024^3) — the convention every cloud's pricing
// page uses for "GB / month".
func bytesToMonthlyCost(b int64, pricePerGBMonth float64) float64 {
	if b <= 0 {
		return 0
	}
	gb := float64(b) / float64(1<<30)
	cost := gb * pricePerGBMonth
	// Round to cents.
	return math.Round(cost*100) / 100
}

// detectAnomalies splits the baseline window into "tail" (last
// AnomalyTailWindow) and "baseline" (rest) and flags deployments
// whose tail rate differs from the baseline rate by >
// AnomalyMultiplier×.
//
// We re-walk because the per-deployment forecast already discards
// the tail/baseline split; a separate pass keeps the data flow
// clean.
func detectAnomalies(ctx context.Context, store *backup.ManifestStore, fcs []DeploymentForecast, opts Options) []GrowthAnomaly {
	var out []GrowthAnomaly
	tailStart := opts.Now.Add(-AnomalyTailWindow)
	for _, fc := range fcs {
		if fc.SamplesObserved < MinSamples {
			continue
		}
		if err := ctx.Err(); err != nil {
			return out
		}
		var tailBytes, baselineBytes int64
		var tailDays, baselineDays float64
		var firstTail, lastTail, firstBase, lastBase time.Time
		for m, lerr := range store.List(ctx, fc.Name, opts.Verifier) {
			if lerr != nil {
				continue
			}
			if m.StoppedAt.Before(opts.Now.Add(-opts.BaselineWindow)) ||
				m.StoppedAt.After(opts.Now) {
				continue
			}
			size := manifestLogicalBytes(m)
			if !m.StoppedAt.Before(tailStart) {
				tailBytes += size
				if firstTail.IsZero() || m.StoppedAt.Before(firstTail) {
					firstTail = m.StoppedAt
				}
				if m.StoppedAt.After(lastTail) {
					lastTail = m.StoppedAt
				}
			} else {
				baselineBytes += size
				if firstBase.IsZero() || m.StoppedAt.Before(firstBase) {
					firstBase = m.StoppedAt
				}
				if m.StoppedAt.After(lastBase) {
					lastBase = m.StoppedAt
				}
			}
		}
		tailDays = anomalySpanDays(firstTail, lastTail)
		baselineDays = anomalySpanDays(firstBase, lastBase)
		if tailDays == 0 || baselineDays == 0 {
			continue
		}
		tailRate := float64(tailBytes) / tailDays
		baselineRate := float64(baselineBytes) / baselineDays
		if baselineRate == 0 {
			continue
		}
		mult := tailRate / baselineRate
		if mult >= AnomalyMultiplier {
			out = append(out, GrowthAnomaly{
				Deployment:          fc.Name,
				Reason:              "sudden_uptick",
				BaselineBytesPerDay: baselineRate,
				RecentBytesPerDay:   tailRate,
				MultiplierObserved:  mult,
				MultiplierThreshold: AnomalyMultiplier,
			})
		} else if mult > 0 && 1.0/mult >= AnomalyMultiplier {
			out = append(out, GrowthAnomaly{
				Deployment:          fc.Name,
				Reason:              "sudden_drop",
				BaselineBytesPerDay: baselineRate,
				RecentBytesPerDay:   tailRate,
				MultiplierObserved:  mult,
				MultiplierThreshold: 1.0 / AnomalyMultiplier,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Deployment < out[j].Deployment })
	return out
}

func anomalySpanDays(first, last time.Time) float64 {
	if first.IsZero() || last.IsZero() {
		return 0
	}
	span := last.Sub(first).Hours() / 24
	if span < 1 {
		span = 1
	}
	return span
}
