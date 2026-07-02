package runner

import (
	"encoding/json"
	"testing"
	"time"
)

// Regression: Result.Duration is a time.Duration (nanoseconds); the frozen
// JSON key duration_ms must carry WHOLE MILLISECONDS. A raw struct tag
// would emit ns under the _ms key, inflating readings 1e6x.
func TestResult_JSON_DurationMS(t *testing.T) {
	r := Result{BackupID: "b1", Duration: 42 * time.Second}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := m["duration_ms"]; got != float64(42000) {
		t.Errorf("duration_ms = %v, want 42000 (milliseconds)", got)
	}
	if _, ok := m["backup_id"]; !ok {
		t.Error("alias marshalling dropped ordinary fields (backup_id missing)")
	}

	var back Result
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if back.Duration != r.Duration || back.BackupID != "b1" {
		t.Errorf("round-trip = %v/%q, want %v/%q", back.Duration, back.BackupID, r.Duration, "b1")
	}
}
