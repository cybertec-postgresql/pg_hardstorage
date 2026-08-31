package gapstate_test

// A gap record that will not parse used to be dropped by List without
// a trace. That mattered because gapstate is what preflightWALGap
// consults to refuse a PITR whose target lands inside a known WAL gap:
// with the record gone, the pre-flight saw "no gap", let the restore
// through, and PG — which cannot tell a hole from the end of the
// archive — ended recovery at the hole, promoted, and reported success
// arbitrarily far behind. The skip was justified in the source by
// "doctor will surface the corruption via the audit-chain integrity
// check separately", which was not true: nothing walks
// wal/<deployment>/gaps/ for parseability.
//
// The skip itself is right for display surfaces — one corrupt record
// must not black out `wal gaps`. What was missing is that a caller
// making a SAFETY decision could not tell "no gaps" from "no gaps I
// could read".

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

func putGap(t *testing.T, s *gapstate.Store, tli uint32, start, end string) {
	t.Helper()
	if _, err := s.Put(context.Background(), gapstate.Record{
		Deployment: "db1", SlotName: "slot", Timeline: tli,
		GapStartLSN: start, GapEndLSN: end, GapBytes: 128,
	}); err != nil {
		t.Fatal(err)
	}
}

func corruptGapObject(t *testing.T, sp storage.StoragePlugin, key string, body []byte) {
	t.Helper()
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
}

// The regression: an unreadable record must be counted, not vanish.
func TestListUnreadable_CountsRecordsThatWillNotParse(t *testing.T) {
	sp := newSP(t)
	at := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	s := gapstate.NewWithClock(sp, fixedClock(at))
	putGap(t, s, 1, "0/1000000", "0/1000100")

	// Two objects at the canonical prefix that do not parse: a
	// truncated write and a hand-edit that lost the closing brace.
	corruptGapObject(t, sp, "wal/db1/gaps/1-99999999-slot.json", []byte(`{"schema":"pg_har`))
	corruptGapObject(t, sp, "wal/db1/gaps/1-99999998-slot.json", []byte(`not json at all`))

	recs, unreadable, err := s.ListUnreadable(context.Background(), "db1")
	if err != nil {
		t.Fatalf("ListUnreadable: %v", err)
	}
	if len(recs) != 1 {
		t.Errorf("parsed %d records, want 1 (the good one must still come back — one corrupt "+
			"record must not black out the rest)", len(recs))
	}
	if unreadable != 2 {
		t.Fatalf("unreadable = %d, want 2 — an unparseable gap record that reports as nothing "+
			"lets the PITR pre-flight conclude \"no gap\" and admit a restore that will "+
			"silently truncate", unreadable)
	}
}

// List keeps its old shape for display callers, and must still skip.
func TestList_StillSkipsUnreadableRecords(t *testing.T) {
	sp := newSP(t)
	s := gapstate.NewWithClock(sp, fixedClock(time.Unix(0, 0).UTC()))
	putGap(t, s, 1, "0/1000000", "0/1000100")
	corruptGapObject(t, sp, "wal/db1/gaps/1-77777777-slot.json", []byte(`{`))

	recs, err := s.List(context.Background(), "db1")
	if err != nil {
		t.Fatalf("List must not fail on one corrupt record: %v", err)
	}
	if len(recs) != 1 {
		t.Errorf("List returned %d records, want 1", len(recs))
	}
}

// A healthy prefix must report zero, or every restore pre-flight warns.
func TestListUnreadable_CleanPrefixReportsZero(t *testing.T) {
	sp := newSP(t)
	s := gapstate.NewWithClock(sp, fixedClock(time.Unix(0, 0).UTC()))
	putGap(t, s, 1, "0/1000000", "0/1000100")
	putGap(t, s, 2, "0/2000000", "0/2000100")

	recs, unreadable, err := s.ListUnreadable(context.Background(), "db1")
	if err != nil {
		t.Fatal(err)
	}
	if unreadable != 0 {
		t.Errorf("unreadable = %d on a clean prefix — the pre-flight would warn on every "+
			"healthy restore", unreadable)
	}
	if len(recs) != 2 {
		t.Errorf("got %d records, want 2", len(recs))
	}
}

// Non-.json objects under the prefix are not gap records and must not
// be counted as corruption.
func TestListUnreadable_IgnoresNonJSONObjects(t *testing.T) {
	sp := newSP(t)
	s := gapstate.NewWithClock(sp, fixedClock(time.Unix(0, 0).UTC()))
	putGap(t, s, 1, "0/1000000", "0/1000100")
	corruptGapObject(t, sp, "wal/db1/gaps/README.txt", []byte("notes"))

	_, unreadable, err := s.ListUnreadable(context.Background(), "db1")
	if err != nil {
		t.Fatal(err)
	}
	if unreadable != 0 {
		t.Errorf("unreadable = %d — a non-.json object is not a gap record", unreadable)
	}
}

func TestListUnreadable_EmptyDeploymentRejected(t *testing.T) {
	s := gapstate.New(newSP(t))
	if _, _, err := s.ListUnreadable(context.Background(), ""); err == nil {
		t.Error("empty deployment must be rejected")
	}
}
