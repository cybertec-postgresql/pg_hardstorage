package gapstate_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

// putGapAt is a small helper: drop a record at the given
// (deployment, tli, detectedAt). Avoids per-test boilerplate.
func putGapAt(t *testing.T, s *gapstate.Store, deployment string, tli uint32, at time.Time) {
	t.Helper()
	rec := gapstate.Record{
		Deployment:  deployment,
		SlotName:    "pg_hardstorage_" + deployment,
		Timeline:    tli,
		GapStartLSN: "0/3000028",
		GapEndLSN:   "0/30001A0",
		GapBytes:    420,
		DetectedAt:  at,
	}
	if _, err := s.Put(context.Background(), rec); err != nil {
		t.Fatalf("seed gap (TLI %d): %v", tli, err)
	}
}

// TestPurgeOrphans_KeepsLiveTLIs: gaps on TLI 7 stay when
// liveTimelines={7}; gaps on TLI 5 (orphan) get reaped.
func TestPurgeOrphans_KeepsLiveTLIs(t *testing.T) {
	s := gapstate.New(newSP(t))
	at := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	putGapAt(t, s, "db1", 5, at)
	putGapAt(t, s, "db1", 7, at.Add(time.Minute))
	putGapAt(t, s, "db1", 7, at.Add(2*time.Minute))

	live := map[uint32]struct{}{7: {}}
	removed, err := s.PurgeOrphans(context.Background(), "db1", live, false)
	if err != nil {
		t.Fatalf("PurgeOrphans: %v", err)
	}
	if len(removed) != 1 {
		t.Errorf("removed %d, want 1", len(removed))
	}
	if removed[0].Timeline != 5 {
		t.Errorf("removed TLI = %d, want 5", removed[0].Timeline)
	}

	// TLI 7 records still there.
	all, _ := s.List(context.Background(), "db1")
	if len(all) != 2 {
		t.Errorf("after purge: %d records left, want 2 (TLI 7 only)", len(all))
	}
	for _, r := range all {
		if r.Timeline != 7 {
			t.Errorf("orphan TLI %d survived", r.Timeline)
		}
	}
}

// TestPurgeOrphans_KeepsNewerTimelineGapForOlderBackup is the
// forward-PITR regression: a gap recorded on a NEWER timeline than any
// live backup must NOT be purged. Recovery replays forward and follows
// timeline switches, so a backup on TLI 1 restored with
// recovery_target_timeline='latest' crosses a gap created on TLI 2 by
// the failover. preflightWALGap checks the target LSN against ALL gaps
// regardless of timeline; the purge must keep any gap >= the lowest live
// backup timeline. The old exact-membership purge wrongly reaped this
// gap (TLI 2 not in liveTimelines={1}) and silently dropped the PITR
// refusal.
func TestPurgeOrphans_KeepsNewerTimelineGapForOlderBackup(t *testing.T) {
	s := gapstate.New(newSP(t))
	at := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	putGapAt(t, s, "db1", 2, at) // failover gap on the NEW timeline
	// Only a TLI-1 backup is live (no backup taken on TLI 2 yet).
	live := map[uint32]struct{}{1: {}}

	removed, err := s.PurgeOrphans(context.Background(), "db1", live, false)
	if err != nil {
		t.Fatalf("PurgeOrphans: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed %d, want 0 (the TLI-2 gap is reachable by a TLI-1 backup's forward PITR)", len(removed))
	}
	all, _ := s.List(context.Background(), "db1")
	if len(all) != 1 || all[0].Timeline != 2 {
		t.Errorf("the TLI-2 gap must survive; got %d records", len(all))
	}

	// Now a gap strictly BELOW every live backup is safely reaped: with
	// live backups on TLI 3 and 5, the TLI-1 and TLI-2 gaps are both
	// below min(live)=3 and unreachable by any forward PITR.
	putGapAt(t, s, "db1", 1, at.Add(time.Minute))
	live2 := map[uint32]struct{}{3: {}, 5: {}}
	removed2, err := s.PurgeOrphans(context.Background(), "db1", live2, false)
	if err != nil {
		t.Fatalf("PurgeOrphans (2): %v", err)
	}
	// TLI 1 and TLI 2 are both < min(live2)=3 → both reaped.
	if len(removed2) != 2 {
		t.Errorf("removed %d, want 2 (TLI 1 and TLI 2, both below min live TLI 3)", len(removed2))
	}
}

// TestPurgeOrphans_DryRun_NoMutation: dry-run identifies the
// orphans without removing them. Re-run real does the work.
func TestPurgeOrphans_DryRun_NoMutation(t *testing.T) {
	s := gapstate.New(newSP(t))
	at := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	putGapAt(t, s, "db1", 3, at)
	putGapAt(t, s, "db1", 7, at.Add(time.Minute))

	live := map[uint32]struct{}{7: {}}
	preview, err := s.PurgeOrphans(context.Background(), "db1", live, true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(preview) != 1 {
		t.Fatalf("preview returned %d, want 1", len(preview))
	}
	all, _ := s.List(context.Background(), "db1")
	if len(all) != 2 {
		t.Errorf("dry-run mutated state; %d records left, want 2", len(all))
	}
	// Real run finishes the job.
	removed, err := s.PurgeOrphans(context.Background(), "db1", live, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Errorf("real run removed %d, want 1", len(removed))
	}
}

// TestPurgeOrphans_RefusesEmptyLiveSet: empty liveTimelines is
// rejected because it would treat every record as an orphan
// and reap the whole tree. Use PurgeAll for that.
func TestPurgeOrphans_RefusesEmptyLiveSet(t *testing.T) {
	s := gapstate.New(newSP(t))
	putGapAt(t, s, "db1", 7, time.Now())
	if _, err := s.PurgeOrphans(context.Background(), "db1", nil, false); err == nil {
		t.Error("nil liveTimelines should refuse")
	}
	if _, err := s.PurgeOrphans(context.Background(), "db1", map[uint32]struct{}{}, false); err == nil {
		t.Error("empty liveTimelines should refuse")
	}
}

// TestPurgeOrphans_NoOrphans_ReturnsEmpty: every TLI is in
// liveTimelines → no removals.
func TestPurgeOrphans_NoOrphans_ReturnsEmpty(t *testing.T) {
	s := gapstate.New(newSP(t))
	putGapAt(t, s, "db1", 7, time.Now())
	live := map[uint32]struct{}{7: {}, 8: {}}
	removed, err := s.PurgeOrphans(context.Background(), "db1", live, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Errorf("removed %d, want 0", len(removed))
	}
}

// TestPurgeAll_RemovesEverything: --all-equivalent reaps the
// full per-deployment gap tree, regardless of TLI membership.
func TestPurgeAll_RemovesEverything(t *testing.T) {
	s := gapstate.New(newSP(t))
	at := time.Now()
	for i, tli := range []uint32{3, 5, 7, 9} {
		putGapAt(t, s, "db1", tli, at.Add(time.Duration(i)*time.Minute))
	}
	removed, err := s.PurgeAll(context.Background(), "db1", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 4 {
		t.Errorf("removed %d, want 4", len(removed))
	}
	all, _ := s.List(context.Background(), "db1")
	if len(all) != 0 {
		t.Errorf("PurgeAll left %d records", len(all))
	}
}

// TestPurgeAll_WipesCorruptRecord: a corrupt/unparseable .json
// under the gaps prefix must still be removed by an end-to-end
// wipe. List() skips unparseable bodies, so a PurgeAll built on
// List would leave the corrupt file behind — this pins the
// raw-key sweep that fixes it.
func TestPurgeAll_WipesCorruptRecord(t *testing.T) {
	sp := newSP(t)
	s := gapstate.New(sp)
	ctx := context.Background()

	// One valid record, one corrupt sibling written straight to
	// storage under the same deployment's gaps prefix.
	putGapAt(t, s, "db1", 7, time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC))
	corruptKey := "wal/db1/gaps/9-1700000000000000000.json"
	if _, err := sp.Put(ctx, corruptKey, bytes.NewReader([]byte("{not valid json")), storage.PutOptions{IfNotExists: true}); err != nil {
		t.Fatalf("seed corrupt record: %v", err)
	}

	// Sanity: List sees only the parseable record (the corrupt
	// one is skipped), which is exactly why a List-based PurgeAll
	// would miss it.
	if all, err := s.List(ctx, "db1"); err != nil {
		t.Fatalf("List: %v", err)
	} else if len(all) != 1 {
		t.Fatalf("List returned %d, want 1 (corrupt skipped)", len(all))
	}

	if _, err := s.PurgeAll(ctx, "db1", false); err != nil {
		t.Fatalf("PurgeAll: %v", err)
	}

	// Nothing — valid or corrupt — may remain under the prefix.
	var leftover []string
	for info, err := range sp.List(ctx, "wal/db1/gaps/") {
		if err != nil {
			t.Fatalf("raw list: %v", err)
		}
		leftover = append(leftover, info.Key)
	}
	if len(leftover) != 0 {
		t.Errorf("PurgeAll left %d file(s) behind: %v", len(leftover), leftover)
	}
}

// TestPurgeAll_DryRun_NoMutation: dry-run preview path.
func TestPurgeAll_DryRun_NoMutation(t *testing.T) {
	s := gapstate.New(newSP(t))
	putGapAt(t, s, "db1", 7, time.Now())
	preview, err := s.PurgeAll(context.Background(), "db1", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview) != 1 {
		t.Errorf("preview = %d, want 1", len(preview))
	}
	all, _ := s.List(context.Background(), "db1")
	if len(all) != 1 {
		t.Errorf("dry-run mutated; %d records left, want 1", len(all))
	}
}

// TestPurgeOrphans_Idempotent: re-running on cleaned state
// returns empty cleanly.
func TestPurgeOrphans_Idempotent(t *testing.T) {
	s := gapstate.New(newSP(t))
	putGapAt(t, s, "db1", 5, time.Now())
	live := map[uint32]struct{}{7: {}}
	if _, err := s.PurgeOrphans(context.Background(), "db1", live, false); err != nil {
		t.Fatal(err)
	}
	removed, err := s.PurgeOrphans(context.Background(), "db1", live, false)
	if err != nil {
		t.Errorf("idempotent re-run: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("re-run removed %d, want 0", len(removed))
	}
}

// TestPurgeOrphans_DeploymentScoped: removing TLI 7 in db1
// doesn't touch TLI 7 in db2.
func TestPurgeOrphans_DeploymentScoped(t *testing.T) {
	s := gapstate.New(newSP(t))
	at := time.Now()
	putGapAt(t, s, "db1", 7, at)
	putGapAt(t, s, "db2", 7, at)

	live := map[uint32]struct{}{99: {}} // 7 is orphan in db1
	if _, err := s.PurgeOrphans(context.Background(), "db1", live, false); err != nil {
		t.Fatal(err)
	}
	// db2 untouched.
	d2, _ := s.List(context.Background(), "db2")
	if len(d2) != 1 {
		t.Errorf("db2 should still have its TLI-7 record; got %d", len(d2))
	}
}
