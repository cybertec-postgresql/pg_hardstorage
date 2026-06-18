// Package anomaly implements a small, dependency-free baseline +
// z-score detector for backup-shape outliers (size, duration, file
// count). The primitive is pure math: it does not read manifests, it
// does not write events, it does not consult PG. Callers (the CLI's
// `anomaly check` command, the future post-commit hook, the doctor's
// future "looks weird" surface) feed it Samples and read Reports.
//
// Why this lives in a separate package: the detector has no
// dependency on storage, manifests, or the audit chain. Keeping it
// dependency-free lets the testkit drive it from synthetic data
// streams without spinning up a repo, and lets future surfaces (the
// LLM helper's "is this backup unusual?" tool, a Grafana exporter)
// reuse it without dragging the backup package in.
//
// The math is intentionally boring:
//
//  1. Compute mean μ and sample stddev σ of each metric over the
//     most-recent N priors of the same deployment + same type.
//  2. For the candidate sample, score = (x - μ) / σ, capped at ±∞
//     (we explicitly handle σ == 0).
//  3. If |score| > threshold, flag it.
//
// We deliberately do NOT implement seasonal / time-of-week
// adjustments, ARIMA, or anything else more sophisticated. Backup
// metrics aren't seasonal in any meaningful way (a backup at 04:00
// and a backup at 16:00 should look the same), and any cleverness
// here is just an opportunity to be wrong. If a customer needs
// fancier baselines they can read this package's outputs into
// whatever forecasting tool they prefer.
package anomaly

import (
	"errors"
	"math"
	"sort"
	"time"
)

// DefaultThreshold is the |z-score| above which a sample is flagged
// as anomalous. 3.0 is the standard 3-sigma rule; ~99.7% of normally-
// distributed samples fall within ±3σ. Operators tuning for noisier
// fleets can raise this; tuning below 2 is asking for false positives.
const DefaultThreshold = 3.0

// DefaultWindow is the rolling-baseline size. Ten same-type prior
// samples is enough to compute a stddev that's not dominated by a
// single old run, while small enough that a recent regime change
// (we doubled the database size last week) ages out of the window
// in a sensible time.
const DefaultWindow = 10

// DefaultMinSamples is the floor for "enough priors to score
// against." A baseline of two samples gives a degenerate stddev that
// produces wildly false positives. Three is the smallest window that
// can discriminate a tight cluster from a single drifting outlier.
const DefaultMinSamples = 3

// Metric names. Stable strings: surfaced in JSON Reports, in audit
// events when the CLI emits an `anomaly.detected` event, and in the
// future Prometheus metric labels.
type Metric string

const (
	// MetricLogicalBytes is the logical (pre-dedup) size of a backup.
	MetricLogicalBytes Metric = "logical_bytes"
	// MetricDurationSeconds is the wall-clock backup duration.
	MetricDurationSeconds Metric = "duration_seconds"
	// MetricFileCount is the number of files in the backup tree.
	MetricFileCount Metric = "file_count"
	// MetricUniqueChunkCount is the count of unique CDC chunks
	// referenced by the backup manifest.
	MetricUniqueChunkCount Metric = "unique_chunk_count"
)

// Sample is the per-backup shape the detector consumes. The fields
// mirror what `list` / `status` already extract from manifests via
// summarizeManifest. The CLI builds these by walking ManifestStore.
type Sample struct {
	BackupID         string    `json:"backup_id"`
	Type             string    `json:"type"`
	StoppedAt        time.Time `json:"stopped_at"`
	LogicalBytes     int64     `json:"logical_bytes"`
	DurationSeconds  float64   `json:"duration_seconds"`
	FileCount        int64     `json:"file_count"`
	UniqueChunkCount int64     `json:"unique_chunk_count"`
}

// Score is the per-metric verdict. Mean + StdDev are surfaced for
// the operator's "okay but how bad" sense-making — without those,
// a score of 4.7 is just a number.
type Score struct {
	Metric  Metric  `json:"metric"`
	Value   float64 `json:"value"`
	Mean    float64 `json:"mean"`
	StdDev  float64 `json:"stddev"`
	Z       float64 `json:"z"`
	AbsZ    float64 `json:"abs_z"`
	Flagged bool    `json:"flagged"`
}

// Report is the detector's per-Sample output. AnyFlagged is the
// summary verdict; Reasons enumerates every metric that crossed the
// threshold (so a CLI summary can list them, not just the first).
type Report struct {
	Schema       string    `json:"schema"`
	Deployment   string    `json:"deployment"`
	BackupID     string    `json:"backup_id"`
	Type         string    `json:"type"`
	BaselineSize int       `json:"baseline_size"`
	Threshold    float64   `json:"threshold"`
	Window       int       `json:"window"`
	Scores       []Score   `json:"scores,omitempty"`
	AnyFlagged   bool      `json:"any_flagged"`
	Reasons      []string  `json:"reasons,omitempty"`
	Skipped      string    `json:"skipped,omitempty"` // non-empty when we refuse to score (low-N, etc.)
	GeneratedAt  time.Time `json:"generated_at"`
}

// Schema is the on-disk version tag for Report bodies.
const Schema = "pg_hardstorage.anomaly.v1"

// Detector holds the tuning. The zero value is "use the defaults"
// — every field validates lazily in Score so a Detector{} is
// immediately usable.
type Detector struct {
	Threshold  float64 // 0 → DefaultThreshold
	Window     int     // 0 → DefaultWindow; <0 disables windowing
	MinSamples int     // 0 → DefaultMinSamples

	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
}

// ErrNotEnoughHistory is the sentinel returned when the prior-sample
// count is below MinSamples. Callers wanting "no anomaly check" can
// errors.Is and treat it as a clean skip.
var ErrNotEnoughHistory = errors.New("anomaly: not enough prior samples")

// Score applies the detector to a candidate Sample against the prior
// list. The prior list does NOT have to be filtered or sorted by the
// caller — Score handles both:
//
//   - Filters to same-Type as the candidate (full vs incremental
//     vs snapshot have different shape; cross-comparison is
//     meaningless).
//   - Sorts by StoppedAt and takes the most-recent Window entries.
//   - Refuses to score (returns Report.Skipped non-empty + a nil
//     error) when the resulting baseline is below MinSamples.
//
// Returns a non-nil Report in every case; err is non-nil only on
// programmer errors (nil receiver, invalid candidate). Domain-level
// "I won't score this" is communicated via Report.Skipped, which is
// what callers want for cron-driven anomaly checks (they want a
// clean exit + structured "couldn't score" reason).
func (d *Detector) Score(deployment string, prior []Sample, candidate Sample) (*Report, error) {
	if d == nil {
		return nil, errors.New("anomaly: nil Detector")
	}
	threshold := d.Threshold
	if threshold == 0 {
		threshold = DefaultThreshold
	}
	window := d.Window
	if window == 0 {
		window = DefaultWindow
	}
	minSamples := d.MinSamples
	if minSamples == 0 {
		minSamples = DefaultMinSamples
	}
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}

	rep := &Report{
		Schema:      Schema,
		Deployment:  deployment,
		BackupID:    candidate.BackupID,
		Type:        candidate.Type,
		Threshold:   threshold,
		Window:      window,
		GeneratedAt: now().UTC(),
	}

	// Filter to same-type priors and sort by StoppedAt descending.
	// Same-type filter is intentional: a full and an incremental
	// have wildly different size profiles; mixing them blows up
	// stddev and produces useless scores.
	var sameType []Sample
	for _, s := range prior {
		if s.Type != candidate.Type {
			continue
		}
		// Don't compare a sample against itself if it accidentally
		// got included in prior. Caller convenience.
		if s.BackupID == candidate.BackupID && !s.StoppedAt.IsZero() {
			continue
		}
		sameType = append(sameType, s)
	}
	sort.Slice(sameType, func(i, j int) bool {
		return sameType[i].StoppedAt.After(sameType[j].StoppedAt)
	})
	if window > 0 && len(sameType) > window {
		sameType = sameType[:window]
	}
	rep.BaselineSize = len(sameType)

	if len(sameType) < minSamples {
		rep.Skipped = "baseline too small (have " +
			itoa(len(sameType)) + " same-type prior(s), need " +
			itoa(minSamples) + ")"
		return rep, nil
	}

	// Score every metric the Sample carries.
	scoreOne := func(metric Metric, getX func(Sample) float64) {
		var xs []float64
		for _, s := range sameType {
			xs = append(xs, getX(s))
		}
		mean, stddev := meanStdDev(xs)
		x := getX(candidate)
		var z float64
		if stddev == 0 {
			// Degenerate case: every prior had identical value.
			// If the candidate matches, score is 0; if it differs,
			// it's "infinitely anomalous" — we use a finite sentinel
			// (signed math.MaxFloat64 / 2) instead of math.Inf so
			// that downstream JSON serialization survives:
			// `encoding/json` rejects ±Inf and NaN per RFC 8259, so
			// emitting +Inf here breaks the audit-chain append with
			//   audit: append "anomaly.detected" failed (chain may
			//   have a gap): json: unsupported value: +Inf
			// and the chain gets a gap.  /2 keeps room for any
			// downstream arithmetic without overflowing to +Inf.
			if x == mean {
				z = 0
			} else {
				z = float64(sign(x-mean)) * (math.MaxFloat64 / 2)
			}
		} else {
			z = (x - mean) / stddev
		}
		absZ := math.Abs(z)
		flagged := absZ > threshold
		rep.Scores = append(rep.Scores, Score{
			Metric:  metric,
			Value:   x,
			Mean:    mean,
			StdDev:  stddev,
			Z:       z,
			AbsZ:    absZ,
			Flagged: flagged,
		})
		if flagged {
			rep.AnyFlagged = true
			rep.Reasons = append(rep.Reasons,
				string(metric)+": value "+fmtFloat(x)+
					" vs baseline mean "+fmtFloat(mean)+
					" (z="+fmtFloat(z)+", threshold ±"+fmtFloat(threshold)+")")
		}
	}

	scoreOne(MetricLogicalBytes, func(s Sample) float64 { return float64(s.LogicalBytes) })
	scoreOne(MetricDurationSeconds, func(s Sample) float64 { return s.DurationSeconds })
	scoreOne(MetricFileCount, func(s Sample) float64 { return float64(s.FileCount) })
	scoreOne(MetricUniqueChunkCount, func(s Sample) float64 { return float64(s.UniqueChunkCount) })

	return rep, nil
}

// meanStdDev returns the arithmetic mean and the sample standard
// deviation of xs. Sample stddev (n-1 denominator) is the right form
// for "we have a finite, sub-population sample and want to estimate
// the underlying population spread" — which is exactly what backup
// history is.
func meanStdDev(xs []float64) (mean, stddev float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean = sum / float64(len(xs))
	if len(xs) < 2 {
		return mean, 0
	}
	var sqSum float64
	for _, x := range xs {
		d := x - mean
		sqSum += d * d
	}
	// n-1 (sample stddev), not n.
	variance := sqSum / float64(len(xs)-1)
	return mean, math.Sqrt(variance)
}

// sign returns 1 for x>0, -1 for x<0, 0 for x==0. Used to pick the
// signed infinity in the σ==0 degenerate path.
func sign(x float64) int {
	switch {
	case x > 0:
		return 1
	case x < 0:
		return -1
	}
	return 0
}

// fmtFloat is a small "render this for human eyeballs" helper. Avoids
// the strconv import-bloat dance for one-off Reason strings; %g would
// require fmt + a Sprintf which is fine but this is leaner.
func fmtFloat(x float64) string {
	if math.IsInf(x, 1) {
		return "+Inf"
	}
	if math.IsInf(x, -1) {
		return "-Inf"
	}
	if math.IsNaN(x) {
		return "NaN"
	}
	// Use 'g' format with limited precision via a tiny ad-hoc
	// rounding to keep Reason strings short and readable.
	rounded := math.Round(x*100) / 100
	return strconvFormatFloat(rounded)
}

// strconvFormatFloat is import-light — we use the math.Round +
// integer split to avoid bringing in strconv just for one helper.
// For the eyeball-friendly precision we want (2 decimals), a pair of
// integer divides is plenty.
func strconvFormatFloat(x float64) string {
	if x == 0 {
		return "0"
	}
	neg := x < 0
	if neg {
		x = -x
	}
	whole := int64(x)
	frac := int64(math.Round((x - float64(whole)) * 100))
	// Carry: math.Round can push frac to 100 for values like 0.999...
	if frac >= 100 {
		whole++
		frac -= 100
	}
	out := itoa(int(whole))
	if frac > 0 {
		out += "."
		if frac < 10 {
			out += "0"
		}
		out += itoa(int(frac))
	}
	if neg {
		out = "-" + out
	}
	return out
}

// itoa is a small base-10 helper; we avoid strconv to keep the
// package's import surface trivially small.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
