package cli

import (
	stdjson "encoding/json"
	"strings"
	"testing"
)

// TestWalRepairBody_V1SchemaShape: regression guard that the
// JSON shape consumers of `wal repair -o json` (and `repair slot`)
// observe is the documented v0.6+ contract. Renaming any of these
// fields would break monitoring scripts and the
// pg_hardstorage.v1 stability commitment.
//
// We marshal a fully-populated body and compare the field set
// (not the values) against the expected v1 keys. New fields can
// be added; existing ones can't be renamed or removed without a
// schema-version bump.
func TestWalRepairBody_V1SchemaShape(t *testing.T) {
	b := walRepairBody{
		Deployment:             "db1",
		Slot:                   "pg_hardstorage_db1",
		Timeline:               2,
		HighestArchived:        "0/3000028",
		Outcome:                "recreated",
		SlotPresent:            true,
		SlotActive:             false,
		SlotRestartLSN:         "0/30001A0",
		SlotMinusArchivedBytes: 100,
		GapDetected:            true,
		GapBytes:               100,
		GapStartLSN:            "0/3000028",
		GapEndLSN:              "0/30001A0",
	}
	raw, err := stdjson.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := stdjson.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"deployment",
		"slot",
		"timeline",
		"highest_archived_lsn",
		"outcome",
		"slot_present",
		"slot_active",
		"slot_restart_lsn",
		"slot_minus_archived_bytes",
		"gap_detected",
		"gap_bytes",
		"gap_start_lsn",
		"gap_end_lsn",
	}
	for _, key := range want {
		if _, ok := got[key]; !ok {
			t.Errorf("missing v1 field: %q (renaming or removing is a contract break)", key)
		}
	}
}

// TestWalRepairBody_OmitemptyOnZero: the new fields use
// `omitempty` (so a "found" outcome with no gap doesn't emit
// gap_bytes / gap_start_lsn / gap_end_lsn). Verify by marshalling
// a zero-gap body and asserting the gap_* keys are absent.
func TestWalRepairBody_OmitemptyOnZero(t *testing.T) {
	b := walRepairBody{
		Deployment:      "db1",
		Slot:            "pg_hardstorage_db1",
		Timeline:        1,
		HighestArchived: "0/0",
		Outcome:         "found",
		SlotPresent:     true,
		// No gap fields populated.
	}
	raw, err := stdjson.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, omitted := range []string{"gap_bytes", "gap_start_lsn", "gap_end_lsn"} {
		if strings.Contains(string(raw), `"`+omitted+`"`) {
			t.Errorf("%s should be omitted when zero (omitempty), but appears in:\n%s",
				omitted, raw)
		}
	}
	// outcome is also tagged omitempty since "" is meaningful
	// (pre-EnsureSlot bodies); confirm it appears when set.
	if !strings.Contains(string(raw), `"outcome":"found"`) {
		t.Errorf("outcome should appear when set:\n%s", raw)
	}
}
