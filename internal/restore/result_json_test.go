package restore

import (
	"encoding/json"
	"testing"
	"time"
)

// Regression: Result.Duration / VerifyResult.Duration are time.Duration
// (nanoseconds); the frozen JSON keys are *_ms and must carry WHOLE
// MILLISECONDS, not ns.
func TestResult_JSON_DurationMS(t *testing.T) {
	r := Result{BackupID: "b1", Duration: 7 * time.Second}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := m["duration_ms"]; got != float64(7000) {
		t.Errorf("duration_ms = %v, want 7000 (milliseconds)", got)
	}
	if _, ok := m["backup_id"]; !ok {
		t.Error("alias marshalling dropped ordinary fields (backup_id missing)")
	}
	var back Result
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if back.Duration != r.Duration {
		t.Errorf("round-trip Duration = %v, want %v", back.Duration, r.Duration)
	}
}

func TestVerifyResult_JSON_DurationMS(t *testing.T) {
	v := VerifyResult{Mode: VerifyRequire, Status: "passed", Duration: 2500 * time.Millisecond}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := m["duration_ms"]; got != float64(2500) {
		t.Errorf("duration_ms = %v, want 2500 (milliseconds)", got)
	}
	var back VerifyResult
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if back.Duration != v.Duration || back.Status != "passed" {
		t.Errorf("round-trip = %v/%q, want %v/%q", back.Duration, back.Status, v.Duration, "passed")
	}
}
