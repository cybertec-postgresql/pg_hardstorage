package restore

// A gap record the pre-flight could not understand must not pass for
// "no gap here".
//
// preflightWALGap already refuses to be silent in two places: a
// gapstate List error emits gap_state_unreadable, and a record that
// fails to JSON-decode is counted and reported the same way, with the
// reasoning spelled out on that path —
//
//	"That is the fail-open direction on the one guard that stops a
//	 silently truncated recovery: PG cannot tell a hole from the end of
//	 the archive, so it ends recovery at the hole, promotes, and reports
//	 success arbitrarily far behind."
//
// checkOneGap had a third way to lose a record, and it was silent:
//
//	start, sErr := pglogrepl.ParseLSN(startStr)
//	if sErr != nil {
//	    return nil // malformed; skip
//	}
//
// The unreadable counter does not cover it. gapstate.Record stores
// gap_start_lsn and gap_end_lsn as plain strings and validates them
// neither on write nor on read, so a record that is perfectly good JSON
// with a garbage LSN is counted READABLE, raises no warning, and is
// then dropped here without a trace. The pre-flight concludes there is
// no gap and lets the PITR through — into the window the record existed
// to describe.
//
// The fix keeps the file's posture (warn, do not block, as for every
// other degraded read) and removes the silence.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func eventBody(events []*output.Event, action string) map[string]any {
	for _, ev := range events {
		if ev != nil && ev.Op == action {
			b, _ := ev.Body.(map[string]any)
			return b
		}
	}
	return nil
}

// A live gap record whose LSNs will not parse: JSON-valid, so the
// unreadable counter never sees it.
func TestPreflightWALGap_MalformedLiveRecordIsReported(t *testing.T) {
	sp := newGapTestSP(t)
	putGap(t, sp, "db1", 7, "not-an-lsn", "0/30001A0", 420,
		time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	emit, seen := collectEvents()

	rec := &Recovery{Enable: true, TargetLSN: "0/3000080"}
	err := preflightWALGap(context.Background(), sp, "db1", "", rec, nil, emit)

	// Posture is unchanged: a record we cannot read must not block a
	// restore, exactly as an unreadable one does not.
	if err != nil {
		t.Fatalf("a malformed gap record blocked the restore: %v", err)
	}
	if !hasEvent(*seen, "gap_record_malformed") {
		t.Fatalf("a gap record with an unparseable start LSN was dropped in silence; "+
			"events seen: %v\n\n"+
			"The pre-flight then reports no gap, and the operator restores into the "+
			"window the record existed to describe. PG cannot tell a hole from the end "+
			"of the archive: it ends recovery at the hole, promotes, and reports success "+
			"arbitrarily far behind.", actionsOf(*seen))
	}
	body := eventBody(*seen, "gap_record_malformed")
	if body == nil {
		t.Fatal("event carried no body")
	}
	if n, _ := body["malformed_records"].(int); n != 1 {
		t.Errorf("malformed_records = %v, want 1", body["malformed_records"])
	}
	// The operator has to be able to find the offending record.
	recs, _ := body["records"].([]string)
	if len(recs) != 1 || !strings.Contains(recs[0], "not-an-lsn") {
		t.Errorf("the report does not identify the bad record: %v", body["records"])
	}
	if !strings.Contains(recs[0], "live") {
		t.Errorf("the report does not name the source: %v", recs[0])
	}
}

// The end LSN is parsed separately; it must be reported too.
func TestPreflightWALGap_MalformedEndLSNIsReported(t *testing.T) {
	sp := newGapTestSP(t)
	putGap(t, sp, "db1", 7, "0/3000028", "garbage-end", 420,
		time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	emit, seen := collectEvents()

	rec := &Recovery{Enable: true, TargetLSN: "0/3000080"}
	if err := preflightWALGap(context.Background(), sp, "db1", "", rec, nil, emit); err != nil {
		t.Fatalf("blocked the restore: %v", err)
	}
	if !hasEvent(*seen, "gap_record_malformed") {
		t.Fatalf("an unparseable END LSN was dropped in silence; events: %v", actionsOf(*seen))
	}
}

// The manifest source runs through the same helper. A malformed record
// there is signed, so it means a defect at backup time — which the
// operator especially needs told.
func TestPreflightWALGap_MalformedManifestRecordIsReported(t *testing.T) {
	sp := newGapTestSP(t)
	emit, seen := collectEvents()

	manifestGaps := []backup.WALGap{{
		SlotName:    "pg_hardstorage_db1",
		SlotRole:    "leader",
		Timeline:    7,
		GapStartLSN: "0/3000028",
		GapEndLSN:   "",
		GapBytes:    420,
		DetectedAt:  time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
	}}
	rec := &Recovery{Enable: true, TargetLSN: "0/3000080"}
	if err := preflightWALGap(context.Background(), sp, "db1", "", rec, manifestGaps, emit); err != nil {
		t.Fatalf("blocked the restore: %v", err)
	}
	if !hasEvent(*seen, "gap_record_malformed") {
		t.Fatalf("a malformed MANIFEST gap record was dropped in silence; events: %v",
			actionsOf(*seen))
	}
	recs, _ := eventBody(*seen, "gap_record_malformed")["records"].([]string)
	if len(recs) != 1 || !strings.Contains(recs[0], "manifest") {
		t.Errorf("the report does not name the manifest source: %v", recs)
	}
}

// Well-formed records must stay quiet, or the warning becomes noise
// operators learn to skip past.
func TestPreflightWALGap_WellFormedRecordsEmitNoMalformedWarning(t *testing.T) {
	sp := newGapTestSP(t)
	putGap(t, sp, "db1", 7, "0/3000028", "0/30001A0", 420,
		time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	emit, seen := collectEvents()

	// Target OUTSIDE the gap, so no refusal either.
	rec := &Recovery{Enable: true, TargetLSN: "0/4000000"}
	if err := preflightWALGap(context.Background(), sp, "db1", "", rec, nil, emit); err != nil {
		t.Fatalf("well-formed non-overlapping gap refused the restore: %v", err)
	}
	if hasEvent(*seen, "gap_record_malformed") {
		t.Errorf("a well-formed gap record raised the malformed warning: %v", actionsOf(*seen))
	}
}

// A malformed record must not mask a DIFFERENT record that does cover
// the target: the refusal still fires, and it is the stronger outcome.
func TestPreflightWALGap_MalformedRecordDoesNotMaskARealRefusal(t *testing.T) {
	sp := newGapTestSP(t)
	putGap(t, sp, "db1", 7, "not-an-lsn", "0/30001A0", 420,
		time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	putGap(t, sp, "db1", 8, "0/3000028", "0/30001A0", 420,
		time.Date(2026, 4, 30, 12, 5, 0, 0, time.UTC))
	emit, _ := collectEvents()

	rec := &Recovery{Enable: true, TargetLSN: "0/3000080"}
	err := preflightWALGap(context.Background(), sp, "db1", "", rec, nil, emit)
	if err == nil {
		t.Fatal("a target inside a well-formed gap was allowed through because another " +
			"record alongside it was malformed")
	}
	if !strings.Contains(err.Error(), "target_in_wal_gap") &&
		!strings.Contains(err.Error(), "WAL gap") {
		t.Errorf("unexpected error: %v", err)
	}
}
