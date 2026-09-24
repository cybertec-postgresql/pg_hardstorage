package restore

import (
	"encoding/json"
	"testing"
	"time"
)

// Regression (M104): Plan.EstimatedRTO is a time.Duration under the
// frozen key estimated_rto_ms; `restore --preview -o json` emitted
// nanoseconds (1e6x inflated). It must carry whole milliseconds and
// round-trip.
func TestPlan_JSON_EstimatedRTOMS(t *testing.T) {
	p := Plan{BackupID: "b1", EstimatedRTO: 7 * time.Second}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, _ := m["estimated_rto_ms"].(float64); got != 7000 {
		t.Fatalf("estimated_rto_ms = %v, want 7000 (json: %s)", m["estimated_rto_ms"], b)
	}
	if m["backup_id"] != "b1" {
		t.Fatalf("other fields lost: %s", b)
	}
	var back Plan
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if back.EstimatedRTO != 7*time.Second {
		t.Fatalf("round-trip EstimatedRTO = %v, want 7s", back.EstimatedRTO)
	}
}
