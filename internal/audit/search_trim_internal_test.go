package audit

import (
	"fmt"
	"testing"
	"time"
)

// Compaction during Search keeps exactly the Limit oldest events (newest
// when Reverse), so the final sort+truncate gives the same answer as
// holding every match.
func TestTrimAuditMatches(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	mk := func() []*Event {
		var evs []*Event
		for i := 9; i >= 0; i-- { // deliberately unsorted
			evs = append(evs, &Event{ID: fmt.Sprintf("e%d", i), Timestamp: t0.Add(time.Duration(i) * time.Second)})
		}
		return evs
	}
	got := trimAuditMatches(mk(), 3, false)
	sortAuditEvents(got)
	if len(got) != 3 || got[0].ID != "e0" || got[2].ID != "e2" {
		t.Fatalf("oldest 3: %v %v %v", got[0].ID, got[1].ID, got[2].ID)
	}
	got = trimAuditMatches(mk(), 3, true)
	sortAuditEvents(got)
	if len(got) != 3 || got[0].ID != "e7" || got[2].ID != "e9" {
		t.Fatalf("newest 3: %v %v %v", got[0].ID, got[1].ID, got[2].ID)
	}
}
