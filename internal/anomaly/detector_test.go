package anomaly_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/anomaly"
)

// makeSamples produces N "full" priors, deterministically spaced one
// hour apart, with the same logical-bytes/duration/file-count
// triplet for each. Useful for "stable baseline" tests.
func makeSamples(n int, lb int64, dur float64, fc int64) []anomaly.Sample {
	out := make([]anomaly.Sample, n)
	t0 := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		out[i] = anomaly.Sample{
			BackupID:         "db1.full." + itoa(i),
			Type:             "full",
			StoppedAt:        t0.Add(time.Duration(i) * time.Hour),
			LogicalBytes:     lb,
			DurationSeconds:  dur,
			FileCount:        fc,
			UniqueChunkCount: fc, // mirror so we don't have to change every callsite
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestDetector_NotEnoughHistory: with fewer than MinSamples priors,
// Score returns a Report with Skipped non-empty and AnyFlagged=false.
// This is the "fresh deployment, only ran 1-2 backups" common case.
func TestDetector_NotEnoughHistory(t *testing.T) {
	d := &anomaly.Detector{}
	candidate := anomaly.Sample{
		BackupID: "db1.full.candidate", Type: "full",
		LogicalBytes: 1_000_000_000, DurationSeconds: 60, FileCount: 100,
	}
	for _, n := range []int{0, 1, 2} {
		prior := makeSamples(n, 1_000_000_000, 60, 100)
		rep, err := d.Score("db1", prior, candidate)
		if err != nil {
			t.Fatalf("n=%d: Score: %v", n, err)
		}
		if rep.Skipped == "" {
			t.Errorf("n=%d: expected Skipped to be non-empty", n)
		}
		if rep.AnyFlagged {
			t.Errorf("n=%d: should not flag with insufficient history", n)
		}
	}
}

// TestDetector_StableBaseline_NoFlag: when the candidate matches the
// baseline within tolerance, no metric flags. The 3-sigma rule means
// even ±2σ is fine.
func TestDetector_StableBaseline_NoFlag(t *testing.T) {
	d := &anomaly.Detector{}
	prior := makeSamples(10, 1_000_000_000, 60, 100)
	// Candidate identical to baseline.
	candidate := anomaly.Sample{
		BackupID: "db1.full.steady", Type: "full",
		StoppedAt:        time.Now(),
		LogicalBytes:     1_000_000_000,
		DurationSeconds:  60,
		FileCount:        100,
		UniqueChunkCount: 100,
	}
	rep, err := d.Score("db1", prior, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if rep.AnyFlagged {
		t.Errorf("identical-to-baseline should not flag; reasons: %v", rep.Reasons)
	}
	if rep.BaselineSize != 10 {
		t.Errorf("BaselineSize=%d, want 10", rep.BaselineSize)
	}
	for _, s := range rep.Scores {
		if s.Flagged {
			t.Errorf("metric %s flagged unexpectedly: %+v", s.Metric, s)
		}
	}
}

// TestDetector_OutlierFlagged: a 10x larger backup obviously breaks
// the baseline. We rely on the actual stddev (which is 0 for a
// constant baseline) → degenerate path uses ±Inf.
func TestDetector_OutlierFlagged(t *testing.T) {
	d := &anomaly.Detector{}
	prior := makeSamples(10, 1_000_000_000, 60, 100)
	candidate := anomaly.Sample{
		BackupID: "db1.full.10x", Type: "full",
		StoppedAt:        time.Now(),
		LogicalBytes:     10_000_000_000, // 10× baseline
		DurationSeconds:  60,
		FileCount:        100,
		UniqueChunkCount: 100,
	}
	rep, err := d.Score("db1", prior, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.AnyFlagged {
		t.Errorf("10x outlier should flag; got Scores=%+v", rep.Scores)
	}
	// Specifically the logical-bytes metric.
	var lbScore *anomaly.Score
	for i := range rep.Scores {
		if rep.Scores[i].Metric == anomaly.MetricLogicalBytes {
			lbScore = &rep.Scores[i]
		}
	}
	if lbScore == nil || !lbScore.Flagged {
		t.Errorf("logical_bytes should be flagged: %+v", lbScore)
	}
}

// TestDetector_OutlierWithSpread: introduce variance in the prior so
// the stddev is non-zero, then a candidate at ~5σ should flag while
// one at ~2σ should not.
func TestDetector_OutlierWithSpread(t *testing.T) {
	// Build priors with known mean=1000, stddev measurable but small.
	// Values: 950, 970, 990, 1010, 1030, 1050 — mean 1000, sample
	// stddev ~37.4.
	prior := []anomaly.Sample{}
	t0 := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	for i, lb := range []int64{950, 970, 990, 1010, 1030, 1050} {
		prior = append(prior, anomaly.Sample{
			BackupID:        "db1.full." + itoa(i),
			Type:            "full",
			StoppedAt:       t0.Add(time.Duration(i) * time.Hour),
			LogicalBytes:    lb,
			DurationSeconds: 60,
			FileCount:       100,
		})
	}
	d := &anomaly.Detector{}

	// Candidate at ~2.5σ — within threshold of 3.0.
	mild := anomaly.Sample{
		BackupID: "db1.full.mild", Type: "full",
		LogicalBytes:    1090, // ~2.4σ above 1000 (stddev ~37.4)
		DurationSeconds: 60, FileCount: 100,
	}
	rep, _ := d.Score("db1", prior, mild)
	for _, s := range rep.Scores {
		if s.Metric == anomaly.MetricLogicalBytes && s.Flagged {
			t.Errorf("mild candidate should NOT flag; got %+v", s)
		}
	}

	// Candidate at ~5σ — well above threshold.
	wild := anomaly.Sample{
		BackupID: "db1.full.wild", Type: "full",
		LogicalBytes:    1200, // ~5.4σ above 1000
		DurationSeconds: 60, FileCount: 100,
	}
	rep2, _ := d.Score("db1", prior, wild)
	if !rep2.AnyFlagged {
		t.Errorf("wild candidate should flag; scores=%+v", rep2.Scores)
	}
}

// TestDetector_TypeFilter: a baseline of incremental backups should
// not be used to score a full backup; the detector filters by Type
// and returns a too-small-baseline skip.
func TestDetector_TypeFilter(t *testing.T) {
	d := &anomaly.Detector{}
	priorIncs := makeSamples(10, 100_000, 30, 50)
	for i := range priorIncs {
		priorIncs[i].Type = "incremental_lsn"
	}
	candidate := anomaly.Sample{
		BackupID: "db1.full.first", Type: "full",
		LogicalBytes: 10_000_000_000, DurationSeconds: 600, FileCount: 1000,
	}
	rep, _ := d.Score("db1", priorIncs, candidate)
	if rep.Skipped == "" {
		t.Error("type-mismatched baseline should produce a skip")
	}
	if rep.AnyFlagged {
		t.Error("should not flag when type-filter empties baseline")
	}
}

// TestDetector_WindowTrim: with Window=3, only the most recent 3
// same-type priors are used. Older outliers shouldn't pollute the
// baseline.
func TestDetector_WindowTrim(t *testing.T) {
	t0 := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	prior := []anomaly.Sample{
		// Old + huge — should be evicted by window trim.
		{BackupID: "old.huge", Type: "full",
			StoppedAt:    t0.Add(-1000 * time.Hour),
			LogicalBytes: 9_999_999_999, DurationSeconds: 600, FileCount: 1000},
		// Three recent + steady.
		{BackupID: "recent.1", Type: "full", StoppedAt: t0.Add(-3 * time.Hour),
			LogicalBytes: 1000, DurationSeconds: 60, FileCount: 100},
		{BackupID: "recent.2", Type: "full", StoppedAt: t0.Add(-2 * time.Hour),
			LogicalBytes: 1000, DurationSeconds: 60, FileCount: 100},
		{BackupID: "recent.3", Type: "full", StoppedAt: t0.Add(-1 * time.Hour),
			LogicalBytes: 1000, DurationSeconds: 60, FileCount: 100},
	}
	d := &anomaly.Detector{Window: 3}
	candidate := anomaly.Sample{
		BackupID: "candidate", Type: "full", StoppedAt: t0,
		LogicalBytes: 1000, DurationSeconds: 60, FileCount: 100,
	}
	rep, _ := d.Score("db1", prior, candidate)
	if rep.BaselineSize != 3 {
		t.Errorf("BaselineSize=%d, want 3 (window trimmed)", rep.BaselineSize)
	}
	if rep.AnyFlagged {
		t.Errorf("recent-only baseline should accept the steady candidate; reasons=%v", rep.Reasons)
	}
}

// TestDetector_DegenerateZeroStddev: when every prior has the
// identical value, stddev=0 and the detector uses ±Inf for any
// non-matching candidate. Make sure infinity propagates cleanly into
// AbsZ, gets flagged, and doesn't NaN the report.
func TestDetector_DegenerateZeroStddev(t *testing.T) {
	d := &anomaly.Detector{}
	prior := makeSamples(5, 1000, 60, 100)
	candidate := anomaly.Sample{
		BackupID: "db1.full.different", Type: "full",
		LogicalBytes: 1500, DurationSeconds: 60, FileCount: 100,
	}
	rep, _ := d.Score("db1", prior, candidate)
	if !rep.AnyFlagged {
		t.Error("zero-stddev + differing value should flag")
	}
	for _, s := range rep.Scores {
		if math.IsNaN(s.Z) {
			t.Errorf("metric %s produced NaN Z: %+v", s.Metric, s)
		}
	}
}

// TestDetector_FilterSelfFromPriors: if the candidate accidentally
// appears in prior (callers building both lists from the same query
// can do this), the detector filters by BackupID so the candidate
// doesn't compare against itself.
func TestDetector_FilterSelfFromPriors(t *testing.T) {
	d := &anomaly.Detector{}
	t0 := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	candidate := anomaly.Sample{
		BackupID: "db1.full.dup", Type: "full",
		StoppedAt:    t0,
		LogicalBytes: 1000, DurationSeconds: 60, FileCount: 100,
	}
	// Build priors that include the candidate.
	prior := makeSamples(10, 1000, 60, 100)
	prior = append(prior, candidate)
	rep, _ := d.Score("db1", prior, candidate)
	// 10 unique priors after the self-filter.
	if rep.BaselineSize != 10 {
		t.Errorf("BaselineSize=%d, want 10 (self should be filtered)", rep.BaselineSize)
	}
}

// TestDetector_ReasonHumanReadable: a flagged report carries Reasons
// that name the metric and include the value+mean. Verifies the
// operator-friendly form so downstream LLM helpers and alerting
// sinks can format the same content cleanly.
func TestDetector_ReasonHumanReadable(t *testing.T) {
	d := &anomaly.Detector{}
	prior := makeSamples(5, 1000, 60, 100)
	candidate := anomaly.Sample{
		BackupID: "db1.full.outlier", Type: "full",
		LogicalBytes: 100000, DurationSeconds: 60, FileCount: 100,
	}
	rep, _ := d.Score("db1", prior, candidate)
	if !rep.AnyFlagged || len(rep.Reasons) == 0 {
		t.Fatalf("expected flagged with reasons; got rep=%+v", rep)
	}
	r := rep.Reasons[0]
	for _, want := range []string{"logical_bytes", "value", "baseline mean", "z="} {
		if !strings.Contains(r, want) {
			t.Errorf("reason missing %q: %s", want, r)
		}
	}
}

// TestDetector_CustomThreshold: tightening the threshold below
// default should flip a previously-clean candidate to flagged.
func TestDetector_CustomThreshold(t *testing.T) {
	prior := []anomaly.Sample{}
	t0 := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	for i, lb := range []int64{1000, 1010, 990, 1020, 980} {
		prior = append(prior, anomaly.Sample{
			BackupID: "p" + itoa(i), Type: "full",
			StoppedAt:    t0.Add(time.Duration(i) * time.Hour),
			LogicalBytes: lb, DurationSeconds: 60, FileCount: 100,
		})
	}
	candidate := anomaly.Sample{
		BackupID: "c", Type: "full",
		LogicalBytes:    1050, // ~3σ above mean 1000 (stddev ~17.3)
		DurationSeconds: 60, FileCount: 100,
	}
	loose := &anomaly.Detector{Threshold: 5.0}
	tight := &anomaly.Detector{Threshold: 1.5}

	repLoose, _ := loose.Score("db1", prior, candidate)
	repTight, _ := tight.Score("db1", prior, candidate)
	if repLoose.AnyFlagged {
		t.Errorf("loose threshold should not flag; %+v", repLoose.Scores)
	}
	if !repTight.AnyFlagged {
		t.Errorf("tight threshold should flag; %+v", repTight.Scores)
	}
}
