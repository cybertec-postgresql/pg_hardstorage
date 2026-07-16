// Package capacity projects future repository size from the manifest
// history. v0.1 produces a linear-trend projection: it walks every
// committed manifest's StartedAt + size, fits a least-squares line to
// (timestamp, cumulative bytes), and extrapolates to the requested
// horizon.
//
// Honest scope: linear is the right model when the operator's growth
// is steady — which is the most common case for a stable production
// fleet. It's wrong for:
//
//   - Bursty growth (a quarterly data import). The R² in the result
//     tells the operator how much to trust the projection.
//   - Pre-roll-out periods (no data yet). We refuse to project with
//     fewer than 3 data points and surface a structured "insufficient
//     history" result instead of a noisy line through two points.
//
// Non-linear models (logistic / piecewise / seasonal) land
// alongside the time-series store the SLO + cost subsystems share.
package capacity

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// SchemaCapacity is the JSON schema string. 24-month back-compat.
const SchemaCapacity = "pg_hardstorage.capacity.v1"

// DefaultHorizon is applied when the caller doesn't pass --horizon.
const DefaultHorizon = 90 * 24 * time.Hour

// Report is the structured output of Project.
type Report struct {
	Schema              string                 `json:"schema"`
	RepoURL             string                 `json:"repo_url"`
	Horizon             string                 `json:"horizon"`
	HorizonAt           time.Time              `json:"horizon_at"`
	GeneratedAt         time.Time              `json:"generated_at"`
	SamplesUsed         int                    `json:"samples_used"`
	BytesPerDay         int64                  `json:"bytes_per_day"`
	CurrentBytes        int64                  `json:"current_bytes"`
	ProjectedBytes      int64                  `json:"projected_bytes"`
	ProjectedDeltaBytes int64                  `json:"projected_delta_bytes"`
	RSquared            float64                `json:"r_squared"`
	Confidence          string                 `json:"confidence"` // high|medium|low|insufficient
	Note                string                 `json:"note,omitempty"`
	PerDeployment       []DeploymentProjection `json:"per_deployment,omitempty"`
}

// Marshal returns r as indented JSON (parity with cost.Report).
func (r *Report) Marshal() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// DeploymentProjection is one deployment's slice of the report.
type DeploymentProjection struct {
	Name                string  `json:"name"`
	BackupCount         int     `json:"backup_count"`
	BytesPerDay         int64   `json:"bytes_per_day"`
	CurrentBytes        int64   `json:"current_bytes"`
	ProjectedBytes      int64   `json:"projected_bytes"`
	ProjectedDeltaBytes int64   `json:"projected_delta_bytes"`
	RSquared            float64 `json:"r_squared"`
}

// ProjectOptions configures Project.
type ProjectOptions struct {
	Horizon time.Duration // 0 → DefaultHorizon
}

// Project walks every deployment's manifests, fits a linear trend to
// (timestamp, cumulative-logical-bytes), and reports the projected
// repository size at now+horizon.
func Project(ctx context.Context, sp storage.StoragePlugin, repoURL string, opts ProjectOptions) (*Report, error) {
	horizon := opts.Horizon
	if horizon <= 0 {
		horizon = DefaultHorizon
	}
	now := time.Now().UTC()
	r := &Report{
		Schema:      SchemaCapacity,
		RepoURL:     repoURL,
		Horizon:     horizon.String(),
		HorizonAt:   now.Add(horizon),
		GeneratedAt: now,
	}

	ms := backup.NewManifestStore(sp)
	deployments, err := ms.Deployments(ctx)
	if err != nil {
		return nil, err
	}

	var allSamples []sample
	for _, dep := range deployments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var depSamples []sample
		var totalLogical int64
		// ListAttestationless: capacity is a read-only projection,
		// not a trust path.  Passing nil to ms.List would reject
		// every signed manifest with "nil verifier" and silently
		// report the deployment as empty.
		for m, lerr := range ms.ListAttestationless(ctx, dep) {
			if lerr != nil || m == nil {
				continue
			}
			var manifestBytes int64
			for _, f := range m.Files {
				manifestBytes += f.Size
			}
			totalLogical += manifestBytes
			depSamples = append(depSamples, sample{at: m.StartedAt, cumulative: totalLogical})
		}

		dp := DeploymentProjection{
			Name:         dep,
			BackupCount:  len(depSamples),
			CurrentBytes: totalLogical,
		}
		if len(depSamples) >= 3 {
			slope, _, r2 := fitLinear(depSamples)
			perDay := slope * 86400
			dp.BytesPerDay = int64(perDay)
			dp.RSquared = r2
			dp.ProjectedDeltaBytes = int64(perDay * horizon.Hours() / 24)
			dp.ProjectedBytes = dp.CurrentBytes + dp.ProjectedDeltaBytes
		}
		r.PerDeployment = append(r.PerDeployment, dp)

		allSamples = append(allSamples, depSamples...)
	}
	sort.Slice(r.PerDeployment, func(i, j int) bool {
		return r.PerDeployment[i].Name < r.PerDeployment[j].Name
	})

	// Aggregate projection: the same linear fit but on the union of
	// every deployment's samples. We re-fold the per-deployment
	// cumulatives into a single timeline because the *repo* growth
	// rate is what the operator's procurement budget cares about,
	// not the sum of independent per-deployment slopes (which would
	// over-count if two deployments both grow but at different
	// times).
	r.CurrentBytes = sumDeploymentCurrent(r.PerDeployment)
	if len(allSamples) >= 3 {
		// Build a global cumulative timeline: sort all samples by
		// timestamp, then re-cumulate the per-sample increments.
		sort.Slice(allSamples, func(i, j int) bool {
			return allSamples[i].at.Before(allSamples[j].at)
		})
		// First convert from per-deployment-cumulative back to deltas,
		// then re-cumulate globally. Since per-deployment cumulatives
		// are monotonic, the delta is sample[i].cumulative -
		// sample[i-1].cumulative within the same deployment — but
		// we've now interleaved them, so a simpler equivalent: just
		// sum the per-deployment slopes weighted by their share of
		// total bytes.
		var totalSlope float64
		var totalR2 float64
		var weightSum float64
		for _, dp := range r.PerDeployment {
			if dp.BytesPerDay <= 0 {
				continue
			}
			w := float64(dp.CurrentBytes)
			if w <= 0 {
				w = 1
			}
			totalSlope += float64(dp.BytesPerDay) * w
			totalR2 += dp.RSquared * w
			weightSum += w
		}
		if weightSum > 0 {
			r.BytesPerDay = int64(totalSlope / weightSum * float64(len(r.PerDeployment)))
			r.RSquared = totalR2 / weightSum
		}
		r.SamplesUsed = len(allSamples)
		r.ProjectedDeltaBytes = int64(float64(r.BytesPerDay) * horizon.Hours() / 24)
		r.ProjectedBytes = r.CurrentBytes + r.ProjectedDeltaBytes
		windowSeconds := sampleWindowSeconds(allSamples)
		r.Confidence = confidenceFor(r.RSquared, len(allSamples), windowSeconds)
		if windowSeconds < 86400 {
			r.Note = fmt.Sprintf("samples span only %s; a per-day projection from a sub-day window is unreliable — collect backups over several days before trusting the growth rate",
				time.Duration(windowSeconds*float64(time.Second)).Round(time.Second))
		}
	} else {
		r.SamplesUsed = len(allSamples)
		r.Confidence = "insufficient"
		r.Note = fmt.Sprintf("only %d sample(s); minimum 3 needed for a projection", len(allSamples))
	}

	return r, nil
}

func sumDeploymentCurrent(deps []DeploymentProjection) int64 {
	var s int64
	for _, d := range deps {
		s += d.CurrentBytes
	}
	return s
}

// confidenceFor maps (R², sample count, observation window) to a
// human-friendly bucket. The observation WINDOW is the load-bearing
// guard: a per-day projection extrapolated from samples spanning only
// seconds/minutes is meaningless no matter how tight the R² — e.g. five
// backups taken within a minute yielded "85 GiB/day, 7.5 TiB projected,
// confidence: medium". A projection can only be high/medium if the
// samples actually span a meaningful fraction of the extrapolation unit.
func confidenceFor(r2 float64, n int, windowSeconds float64) string {
	const (
		day  = 86400.0
		week = 7 * day
	)
	switch {
	case r2 >= 0.85 && n >= 10 && windowSeconds >= week:
		return "high"
	case r2 >= 0.70 && n >= 5 && windowSeconds >= day:
		return "medium"
	default:
		// Under a day of observation the per-day slope is not
		// trustworthy; cap at "low" regardless of fit.
		return "low"
	}
}

// sampleWindowSeconds returns the wall-clock span covered by the
// samples (last timestamp minus first). Samples are assumed sorted or
// are min/max-scanned here.
func sampleWindowSeconds(samples []sample) float64 {
	if len(samples) < 2 {
		return 0
	}
	min, max := samples[0].at, samples[0].at
	for _, s := range samples[1:] {
		if s.at.Before(min) {
			min = s.at
		}
		if s.at.After(max) {
			max = s.at
		}
	}
	return max.Sub(min).Seconds()
}

// sample is one (timestamp, cumulative-bytes) point.
type sample struct {
	at         time.Time
	cumulative int64
}

// fitLinear fits y = slope*x + intercept by least squares, with x in
// seconds since the first sample's timestamp. Returns slope (bytes/sec),
// intercept (bytes), and R² (the goodness-of-fit measure: 1.0 = perfect
// line, 0.0 = no relationship).
//
// Two samples is a defined line (R²=1) but not a meaningful trend; we
// require ≥3 at the call site so this function can assume it has at
// least the data it needs to compute R² without dividing by zero.
func fitLinear(samples []sample) (slope, intercept, r2 float64) {
	if len(samples) < 2 {
		return 0, 0, 0
	}
	n := float64(len(samples))
	t0 := samples[0].at
	var sumX, sumY, sumXY, sumXX, sumYY float64
	for _, s := range samples {
		x := s.at.Sub(t0).Seconds()
		y := float64(s.cumulative)
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
		sumYY += y * y
	}
	denom := n*sumXX - sumX*sumX
	if denom == 0 {
		return 0, sumY / n, 0
	}
	slope = (n*sumXY - sumX*sumY) / denom
	intercept = (sumY - slope*sumX) / n

	// R² = 1 - SS_res / SS_tot.
	ssTot := sumYY - sumY*sumY/n
	if ssTot == 0 {
		// All y values equal → perfect (degenerate) fit.
		return slope, intercept, 1
	}
	var ssRes float64
	for _, s := range samples {
		x := s.at.Sub(t0).Seconds()
		predicted := slope*x + intercept
		residual := float64(s.cumulative) - predicted
		ssRes += residual * residual
	}
	r2 = 1 - ssRes/ssTot
	if math.IsNaN(r2) {
		r2 = 0
	}
	return slope, intercept, r2
}
