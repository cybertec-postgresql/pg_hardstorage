package cli

// agent_frontier_test.go — the lastConfirmedLSN the Patroni coordinator
// hands to EnsureSlot.
//
// This is the second half of the post-promotion timeline bug. The first
// half was `wal stream` resuming past the old timeline's frontier
// (wal_resume_timeline_test.go). This one is the agent's Patroni-aware
// path, and it fails in the mirror-image way: it does not lose the WAL,
// it loses the KNOWLEDGE that WAL was lost.
//
// follower.Coordinator invokes LastConfirmedLSN on leader-change events
// and passes the NEW leader's timeline. Nothing is archived under that
// timeline yet, so a lookup scoped to it misses, and the old code
// returned zero — which EnsureSlot reads as "first-time bootstrap".
// populateGap short-circuits at lastConfirmedLSN == 0, so:
//
//   - GapBytes is 0 and no gap event is emitted;
//   - nothing is written to the GapStore;
//   - restore/gapcheck later has no record with which to refuse an
//     unsafe PITR.
//
// So on the first reconcile after EVERY promotion — the one moment the
// gap calculation exists for — it was guaranteed to measure nothing.
// A real failover gap and a clean handover produced identical output.

import (
	"context"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
)

// TestArchiveFrontierForLeader_AfterPromotionUsesThePriorTimeline is
// the regression test.
func TestArchiveFrontierForLeader_AfterPromotionUsesThePriorTimeline(t *testing.T) {
	sp, _ := newFsRepo(t)
	const dep = "db1"
	for n := uint64(0); n <= 10; n++ {
		putRealSeg(t, sp, dep, 1, n)
	}

	// The coordinator passes the NEW leader's timeline.
	got, err := archiveFrontierForLeader(context.Background(), sp, dep, 2)
	if err != nil {
		t.Fatalf("archiveFrontierForLeader: %v", err)
	}
	want := pglogrepl.LSN(11 * uint64(walsink.SegmentSize))
	if got != want {
		t.Fatalf("frontier = %s, want %s (the end of TLI 1's highest archived segment).\n\n"+
			"Returning 0 here tells EnsureSlot \"first-time bootstrap\", and populateGap "+
			"short-circuits at 0. The gap calculation would then measure nothing on the "+
			"first reconcile after every promotion — the exact event it exists for. No gap "+
			"event, nothing in the GapStore, and no record for restore/gapcheck to refuse "+
			"an unsafe PITR with.", got, want)
	}
}

// TestArchiveFrontierForLeader_CurrentTimelineWins: once the new
// timeline has segments of its own, that is the frontier. The fallback
// must not pin the value to the old timeline forever.
func TestArchiveFrontierForLeader_CurrentTimelineWins(t *testing.T) {
	sp, _ := newFsRepo(t)
	const dep = "db1"
	for n := uint64(0); n <= 10; n++ {
		putRealSeg(t, sp, dep, 1, n)
	}
	for n := uint64(11); n <= 20; n++ {
		putRealSeg(t, sp, dep, 2, n)
	}

	got, err := archiveFrontierForLeader(context.Background(), sp, dep, 2)
	if err != nil {
		t.Fatalf("archiveFrontierForLeader: %v", err)
	}
	want := pglogrepl.LSN(21 * uint64(walsink.SegmentSize))
	if got != want {
		t.Errorf("frontier = %s, want %s — TLI 2 has archived through #20, so the "+
			"fallback must not fire", got, want)
	}
}

// TestArchiveFrontierForLeader_GenuineBootstrapIsStillZero: the zero
// return has a real meaning and it must survive. An empty repository is
// a first-time bootstrap, and EnsureSlot's handling of that is correct.
func TestArchiveFrontierForLeader_GenuineBootstrapIsStillZero(t *testing.T) {
	sp, _ := newFsRepo(t)
	for _, tli := range []uint32{1, 2, 9} {
		got, err := archiveFrontierForLeader(context.Background(), sp, "db1", tli)
		if err != nil {
			t.Fatalf("tli %d: %v", tli, err)
		}
		if got != 0 {
			t.Errorf("tli %d: frontier = %s on an empty repository, want 0 — nothing is "+
				"archived anywhere, so this really is a bootstrap and EnsureSlot should "+
				"treat it as one", tli, got)
		}
	}
}

// TestArchiveFrontierForLeader_NearestTimelineBelow: same reasoning as
// the stream resume. WAL on an older timeline past the branch point is
// diverged history, not the frontier.
func TestArchiveFrontierForLeader_NearestTimelineBelow(t *testing.T) {
	sp, _ := newFsRepo(t)
	const dep = "db1"
	for n := uint64(0); n <= 40; n++ {
		putRealSeg(t, sp, dep, 1, n)
	}
	for n := uint64(0); n <= 10; n++ {
		putRealSeg(t, sp, dep, 2, n)
	}

	got, err := archiveFrontierForLeader(context.Background(), sp, dep, 3)
	if err != nil {
		t.Fatalf("archiveFrontierForLeader: %v", err)
	}
	want := pglogrepl.LSN(11 * uint64(walsink.SegmentSize))
	if got != want {
		t.Errorf("frontier = %s, want %s (TLI 2's frontier).\n\nTLI 1 reaches #41, but "+
			"everything past TLI 2's branch point was written by a primary that was "+
			"subsequently fenced. Treating that as the confirmed position would compute a "+
			"gap against bytes that must never be replayed.", got, want)
	}
}
