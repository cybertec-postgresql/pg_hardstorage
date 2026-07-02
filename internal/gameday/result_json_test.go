package gameday

import (
	"encoding/json"
	"testing"
	"time"
)

// Regression: Duration/RecoveryTime are time.Duration (nanoseconds); the
// frozen JSON keys are *_ms and must carry WHOLE MILLISECONDS, not ns.
func TestResult_JSON_DurationsAreMilliseconds(t *testing.T) {
	r := Result{
		Schema:       SchemaResult,
		Scenario:     "agent_kill",
		Pass:         true,
		Duration:     5 * time.Second,
		RecoveryTime: 1500 * time.Millisecond,
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := m["duration_ms"]; got != float64(5000) {
		t.Errorf("duration_ms = %v, want 5000 (milliseconds, not nanoseconds)", got)
	}
	if got := m["recovery_time_ms"]; got != float64(1500) {
		t.Errorf("recovery_time_ms = %v, want 1500", got)
	}

	var back Result
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if back.Duration != r.Duration || back.RecoveryTime != r.RecoveryTime {
		t.Errorf("round-trip durations = %v/%v, want %v/%v",
			back.Duration, back.RecoveryTime, r.Duration, r.RecoveryTime)
	}
}
