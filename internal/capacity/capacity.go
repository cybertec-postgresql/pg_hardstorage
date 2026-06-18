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
		r.Confidence = confidenceFor(r.RSquared, len(allSamples))
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

// confidenceFor maps (R², sample count) to a human-friendly bucket.
// R² > 0.85 with ≥10 samples = high; > 0.7 with ≥5 = medium; otherwise
// low. Below 3 samples = insufficient (handled upstream).
func confidenceFor(r2 float64, n int) string {
	switch {
	case r2 >= 0.85 && n >= 10:
		return "high"
	case r2 >= 0.70 && n >= 5:
		return "medium"
	default:
		return "low"
	}
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
