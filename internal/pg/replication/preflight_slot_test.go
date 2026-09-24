package replication

import "testing"

// `wal stream` runs Preflight on every (re)connect attempt. On a server
// sized exactly for its slots, the streamer's OWN existing slot is one
// of the occupants, so the table is "full" by construction — and the
// finding was fatal, so the streamer could never restart. A full table
// must only be fatal when a NEW slot has to be created.
func TestSlotTableFinding(t *testing.T) {
	cases := []struct {
		name     string
		max, cur int
		own      bool
		wantCode string // "" = no finding
	}{
		{"room left", 10, 3, false, ""},
		{"full, own slot among them", 2, 2, true, ""},
		{"full, must create", 2, 2, false, "max_replication_slots.full"},
		{"zero slots", 0, 0, false, "max_replication_slots.zero"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := slotTableFinding(tc.max, tc.cur, tc.own)
			got := ""
			if f != nil {
				got = f.Code
			}
			if got != tc.wantCode {
				t.Fatalf("finding = %q, want %q", got, tc.wantCode)
			}
		})
	}
}
