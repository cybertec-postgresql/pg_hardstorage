package gapstate_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

// newSP builds a temp file:// storage plugin.
func newSP(t *testing.T) storage.StoragePlugin {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

// fixedClock returns a clock that always reports `at`. Used
// to generate deterministic record keys in tests.
func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// TestPut_RoundTrip: Put a record, List it back, fields match.
func TestPut_RoundTrip(t *testing.T) {
	at := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	s := gapstate.NewWithClock(newSP(t), fixedClock(at))

	rec := gapstate.Record{
		Deployment:  "db1",
		SlotName:    "pg_hardstorage_db1",
		SlotRole:    "leader",
		Timeline:    2,
		GapStartLSN: "0/3000028",
		GapEndLSN:   "0/30001A0",
		GapBytes:    420,
	}
	key, err := s.Put(context.Background(), rec)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if key == "" {
		t.Error("Put returned empty key")
	}
	want := "wal/db1/gaps/2-" // prefix only — exact unix-nanos depends on time
	if !startsWith(key, want) {
		t.Errorf("key %q should start with %q", key, want)
	}

	got, err := s.List(context.Background(), "db1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List len = %d, want 1", len(got))
	}
	r := got[0]
	if r.Deployment != rec.Deployment {
		t.Errorf("Deployment = %q", r.Deployment)
	}
	if r.SlotName != rec.SlotName {
		t.Errorf("SlotName = %q", r.SlotName)
	}
	if r.Timeline != rec.Timeline {
		t.Errorf("Timeline = %d", r.Timeline)
	}
	if r.GapBytes != rec.GapBytes {
		t.Errorf("GapBytes = %d", r.GapBytes)
	}
	if !r.DetectedAt.Equal(at) {
		t.Errorf("DetectedAt = %v, want %v", r.DetectedAt, at)
	}
	if r.Schema != gapstate.Schema {
		t.Errorf("Schema = %q, want %q", r.Schema, gapstate.Schema)
	}
}

// TestPut_RejectsZeroGap: a zero-byte gap is not a real gap;
// we refuse to record it (avoids a false positive in
// doctor's surface).
func TestPut_RejectsZeroGap(t *testing.T) {
	s := gapstate.New(newSP(t))
	_, err := s.Put(context.Background(), gapstate.Record{
		Deployment: "db1", SlotName: "s", Timeline: 1,
		GapBytes: 0,
	})
	if err == nil {
		t.Error("expected error for zero-byte gap")
	}
}

// TestPut_RejectsValidationGuards: required-field guards.
func TestPut_RejectsValidationGuards(t *testing.T) {
	s := gapstate.New(newSP(t))
	cases := []gapstate.Record{
		{Deployment: "", SlotName: "s", Timeline: 1, GapBytes: 1},    // no deployment
		{Deployment: "db1", SlotName: "", Timeline: 1, GapBytes: 1},  // no slot
		{Deployment: "db1", SlotName: "s", Timeline: 0, GapBytes: 1}, // tli 0
	}
	for i, c := range cases {
		if _, err := s.Put(context.Background(), c); err == nil {
			t.Errorf("case %d: expected error for record %+v", i, c)
		}
	}
}

// TestList_NewestFirst: multiple records with different
// DetectedAt come back newest-first.
func TestList_NewestFirst(t *testing.T) {
	sp := newSP(t)
	earlier := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	later := time.Date(2026, 4, 30, 13, 0, 0, 0, time.UTC)

	s1 := gapstate.NewWithClock(sp, fixedClock(earlier))
	if _, err := s1.Put(context.Background(), gapstate.Record{
		Deployment: "db1", SlotName: "a", Timeline: 1, GapBytes: 100,
	}); err != nil {
		t.Fatal(err)
	}
	s2 := gapstate.NewWithClock(sp, fixedClock(later))
	if _, err := s2.Put(context.Background(), gapstate.Record{
		Deployment: "db1", SlotName: "b", Timeline: 1, GapBytes: 200,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := gapstate.New(sp).List(context.Background(), "db1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if !got[0].DetectedAt.Equal(later) {
		t.Errorf("[0].DetectedAt = %v, want %v (newest first)", got[0].DetectedAt, later)
	}
	if !got[1].DetectedAt.Equal(earlier) {
		t.Errorf("[1].DetectedAt = %v, want %v", got[1].DetectedAt, earlier)
	}
}

// TestLatest_FilterByTimeline: Latest scopes to the
// supplied TLI, ignoring records on other timelines.
func TestLatest_FilterByTimeline(t *testing.T) {
	sp := newSP(t)
	at1 := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	at2 := time.Date(2026, 4, 30, 13, 0, 0, 0, time.UTC)

	s1 := gapstate.NewWithClock(sp, fixedClock(at1))
	if _, err := s1.Put(context.Background(), gapstate.Record{
		Deployment: "db1", SlotName: "a", Timeline: 2, GapBytes: 100,
	}); err != nil {
		t.Fatal(err)
	}
	s2 := gapstate.NewWithClock(sp, fixedClock(at2))
	if _, err := s2.Put(context.Background(), gapstate.Record{
		Deployment: "db1", SlotName: "b", Timeline: 3, GapBytes: 200,
	}); err != nil {
		t.Fatal(err)
	}

	store := gapstate.New(sp)
	// TLI 2: should return the at1 record.
	got, found, err := store.Latest(context.Background(), "db1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("Latest(TLI=2) found = false")
	}
	if got.GapBytes != 100 {
		t.Errorf("Latest(TLI=2).GapBytes = %d, want 100", got.GapBytes)
	}

	// TLI 99: no records.
	_, found, err = store.Latest(context.Background(), "db1", 99)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("Latest(TLI=99) should be not-found")
	}
}

// TestLatestAny: returns the newest record across ALL TLIs.
// Validates the "doctor headline" use case.
func TestLatestAny(t *testing.T) {
	sp := newSP(t)
	at1 := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	at2 := time.Date(2026, 4, 30, 13, 0, 0, 0, time.UTC)

	s1 := gapstate.NewWithClock(sp, fixedClock(at1))
	if _, err := s1.Put(context.Background(), gapstate.Record{
		Deployment: "db1", SlotName: "a", Timeline: 2, GapBytes: 100,
	}); err != nil {
		t.Fatal(err)
	}
	s2 := gapstate.NewWithClock(sp, fixedClock(at2))
	if _, err := s2.Put(context.Background(), gapstate.Record{
		Deployment: "db1", SlotName: "b", Timeline: 5, GapBytes: 200,
	}); err != nil {
		t.Fatal(err)
	}

	got, found, err := gapstate.New(sp).LatestAny(context.Background(), "db1")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("LatestAny found = false")
	}
	if got.Timeline != 5 {
		t.Errorf("LatestAny.Timeline = %d, want 5 (newest)", got.Timeline)
	}
}

// TestList_DeploymentScoped: records on db2 don't leak into
// db1's view.
func TestList_DeploymentScoped(t *testing.T) {
	sp := newSP(t)
	at := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	s := gapstate.NewWithClock(sp, fixedClock(at))
	if _, err := s.Put(context.Background(), gapstate.Record{
		Deployment: "db1", SlotName: "a", Timeline: 1, GapBytes: 100,
	}); err != nil {
		t.Fatal(err)
	}
	at2 := at.Add(time.Hour)
	s2 := gapstate.NewWithClock(sp, fixedClock(at2))
	if _, err := s2.Put(context.Background(), gapstate.Record{
		Deployment: "db2", SlotName: "b", Timeline: 1, GapBytes: 200,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := gapstate.New(sp).List(context.Background(), "db1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Deployment != "db1" {
		t.Errorf("List(db1) = %+v, want only db1's record", got)
	}
}

// TestPut_DefaultsDetectedAtToClock: when DetectedAt is zero
// the store's clock supplies the value. Important for the
// production use case where the Coordinator hands the store
// an unstamped Record.
func TestPut_DefaultsDetectedAtToClock(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := gapstate.NewWithClock(newSP(t), fixedClock(at))
	if _, err := s.Put(context.Background(), gapstate.Record{
		Deployment: "db1", SlotName: "s", Timeline: 1, GapBytes: 1,
		// DetectedAt deliberately zero
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.List(context.Background(), "db1")
	if !got[0].DetectedAt.Equal(at) {
		t.Errorf("DetectedAt = %v, want %v (clock default)", got[0].DetectedAt, at)
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
