package anomaly_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/anomaly"
)

// TestDetector_ZeroVariance_ExplicitVerdict is the regression for the
// σ == 0 path: it used to emit z = ±MaxFloat64/2 (a number no reader
// can interpret, and one that JSON-serialises as 8.98e+307) and the
// Reason text rendered it through an int64 conversion that overflowed
// into "z=-". A constant baseline has no z-score; the report must say
// so explicitly and still flag a deviating candidate.
func TestDetector_ZeroVariance_ExplicitVerdict(t *testing.T) {
	d := &anomaly.Detector{}
	prior := makeSamples(5, 1000, 60, 100)
	candidate := anomaly.Sample{
		BackupID: "db1.full.bigger", Type: "full", StoppedAt: time.Now(),
		LogicalBytes: 1500, DurationSeconds: 60, FileCount: 100, UniqueChunkCount: 100,
	}
	rep, err := d.Score("db1", prior, candidate)
	if err != nil {
		t.Fatal(err)
	}
	var lb *anomaly.Score
	for i := range rep.Scores {
		s := &rep.Scores[i]
		if math.Abs(s.Z) > 1e6 || math.Abs(s.AbsZ) > 1e6 {
			t.Errorf("metric %s: z = %g, abs_z = %g — sentinel leaked into the report", s.Metric, s.Z, s.AbsZ)
		}
		if s.Metric == anomaly.MetricLogicalBytes {
			lb = s
		}
	}
	if lb == nil || !lb.Flagged || !lb.ZeroVariance {
		t.Fatalf("logical_bytes must be flagged with zero_variance: %+v", lb)
	}
	if len(rep.Reasons) != 1 {
		t.Fatalf("Reasons = %q, want exactly the logical_bytes reason", rep.Reasons)
	}
	reason := rep.Reasons[0]
	if strings.Contains(reason, "z=-") || strings.Contains(reason, "z=)") {
		t.Errorf("garbage z in reason: %q", reason)
	}
	if !strings.Contains(reason, "zero variance") {
		t.Errorf("reason should name the zero-variance case: %q", reason)
	}
	if _, err := json.Marshal(rep); err != nil {
		t.Errorf("report must JSON-serialise: %v", err)
	}

	// Matching candidate on a constant baseline: not flagged, and
	// still marked zero-variance so a reader knows z carries no spread.
	candidate.LogicalBytes = 1000
	rep, _ = d.Score("db1", prior, candidate)
	if rep.AnyFlagged {
		t.Errorf("matching candidate flagged: %q", rep.Reasons)
	}
	for _, s := range rep.Scores {
		if !s.ZeroVariance {
			t.Errorf("metric %s: ZeroVariance = false on a constant baseline", s.Metric)
		}
	}
}

// TestDetector_ZeroVariance_BelowMeanReasonReadable: a candidate
// BELOW a constant baseline produced the negative sentinel, which is
// where the int64 overflow printed "z=-". The value and baseline must
// both be readable in the reason.
func TestDetector_ZeroVariance_BelowMeanReasonReadable(t *testing.T) {
	d := &anomaly.Detector{}
	prior := makeSamples(5, 1000, 60, 100)
	candidate := anomaly.Sample{
		BackupID: "db1.full.smaller", Type: "full", StoppedAt: time.Now(),
		LogicalBytes: 10, DurationSeconds: 60, FileCount: 100, UniqueChunkCount: 100,
	}
	rep, _ := d.Score("db1", prior, candidate)
	if len(rep.Reasons) != 1 {
		t.Fatalf("Reasons = %q", rep.Reasons)
	}
	r := rep.Reasons[0]
	if !strings.Contains(r, "value 10 ") || !strings.Contains(r, "1000") {
		t.Errorf("reason lost the numbers: %q", r)
	}
	if strings.Contains(r, "z=-") {
		t.Errorf("garbage z in reason: %q", r)
	}
}
