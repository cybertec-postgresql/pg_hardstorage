package cli

// wal_resume_timeline_test.go — where `wal stream` resumes after a
// promotion.
//
// This is the sharpest data-loss shape found in the streaming path, and
// it is entirely silent.
//
// resolveStartLSN picks its resume point from
// inventory.HighestArchivedLSN(deployment, timeline), which is scoped
// to ONE timeline. After a failover the new primary answers
// IDENTIFY_SYSTEM with timeline N+1, and nothing has been archived
// under N+1 yet — so the lookup misses. The miss used to fall straight
// through to the branch commented "Fresh deployment: no committed
// segments yet", which anchors at the slot's restart_lsn: the new
// leader's CURRENT position.
//
// Every byte between the old timeline's archived frontier and the new
// leader's position is therefore never requested. Nothing reports it:
//
//   - EnsureSlot is called with lastConfirmedLSN=0 (wal.go, ensureSlot),
//     so populateGap short-circuits and GapBytes is always 0;
//   - the fresh branch does not call assertStartGEQRestart, which is
//     the check that raises wal.start_before_slot_restart_lsn;
//   - walsink's contiguity guard resets on every reconnect, because a
//     new Sink is constructed per attempt, so the first record after a
//     reconnect is accepted at any LSN at all;
//   - `wal audit`'s findGaps skips timeline transitions outright.
//
// The stream reports success with resume_strategy
// "fresh-slot-restart-lsn". The hole surfaces later, as a PITR that
// cannot cross the window.
//
// A deployment running only `wal stream` — the documented streaming-only
// HA posture — has no other producer. internal/wal/follower.Coordinator
// does compute a real gap, but it is wired solely from the `agent`
// command, and its own package doc says it does not stream WAL.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
)

// seg returns the starting LSN of segment n at the default size.
func seg(n uint64) pglogrepl.LSN {
	return pglogrepl.LSN(n * uint64(walsink.SegmentSize))
}

// slotAt builds a SlotInfo whose restart_lsn is at the start of segment n.
func slotAt(n uint64) *replication.SlotInfo {
	return &replication.SlotInfo{RestartLSN: seg(n).String()}
}

// TestResolveStartLSN_AfterPromotionResumesAtOldFrontierNotSlot is the
// regression test for the bug.
//
// TLI 1 holds segments 0..10, so the frontier is the end of segment 10
// (= the start of segment 11). The cluster is promoted to TLI 2 and the
// new leader's slot sits at segment 50 — it has already recycled past
// the frontier, so segments 11..49 exist nowhere.
//
// That is genuine, unrecoverable loss, and the only acceptable
// behaviour is to say so. Anchoring at segment 50 and reporting success
// is what this test forbids.
func TestResolveStartLSN_AfterPromotionResumesAtOldFrontierNotSlot(t *testing.T) {
	sp, _ := newFsRepo(t)
	const dep = "db1"
	for n := uint64(0); n <= 10; n++ {
		putRealSeg(t, sp, dep, 1, n)
	}

	var events []*output.Event
	lsn, note, err := resolveStartLSN(context.Background(), sp,
		walStreamOptions{deployment: dep, pgConn: "x"}, 2, slotAt(50),
		func(ev *output.Event) { events = append(events, ev) })

	if err != nil {
		t.Fatalf("resolveStartLSN refused: %v\n\nThe essential property survives "+
			"attempt-first: the resume must NOT skip to the slot at segment 50 — it "+
			"resumes at the OLD timeline's frontier so nothing is silently skipped.", err)
	}
	if lsn != pglogrepl.LSN(11*walsink.SegmentSize) {
		t.Fatalf("start = %s (%s), want the old timeline's frontier (end of segment 10).\n\n"+
			"Resuming anywhere higher skips segments 11..49 silently: EnsureSlot runs "+
			"with lastConfirmedLSN=0 so no gap is computed, walsink's contiguity guard "+
			"resets on reconnect, and `wal audit` skips timeline transitions.", lsn, note)
	}
	warned := false
	for _, ev := range events {
		if ev.Op == "start_behind_slot_restart_lsn" {
			warned = true
		}
	}
	if !warned {
		t.Error("resuming below the recreated slot's restart_lsn must WARN — the mismatch " +
			"is the operator's early signal if PostgreSQL later reports the WAL removed")
	}
}

// TestResolveStartLSN_AfterPromotionResumesAtTheOldFrontier is the
// other half, and the one that actually saves the data.
//
// Same promotion, but the new leader still retains everything: its slot
// sits at segment 4, well behind TLI 1's frontier at segment 11. Nothing
// is lost, and the resume must collect segments 11 onward rather than
// jumping to the leader's position.
//
// Starting at the old timeline's frontier is safe because the two
// lineages share history up to the branch point — the new primary can
// serve those bytes.
func TestResolveStartLSN_AfterPromotionResumesAtTheOldFrontier(t *testing.T) {
	sp, _ := newFsRepo(t)
	const dep = "db1"
	for n := uint64(0); n <= 10; n++ {
		putRealSeg(t, sp, dep, 1, n)
	}

	lsn, note, err := resolveStartLSN(context.Background(), sp,
		walStreamOptions{deployment: dep, pgConn: "x"}, 2, slotAt(4), nil)
	if err != nil {
		t.Fatalf("resolveStartLSN: %v", err)
	}
	if want := seg(11); lsn != want {
		t.Errorf("resume LSN = %s (%s), want %s — the end of TLI 1's highest archived "+
			"segment.\n\nThe slot is at segment 4, so the new primary still holds "+
			"everything from the frontier onward. Anchoring anywhere ahead of %s "+
			"discards WAL that is sitting there for the taking.",
			lsn, note, want, want)
	}
	if !strings.Contains(note, "timeline") {
		t.Errorf("resume_strategy = %q; it lands in the `starting` event body and is the "+
			"only operator-visible record of which branch ran. A post-promotion resume "+
			"reported as a fresh-deployment start hides the most interesting thing that "+
			"just happened.", note)
	}
}

// TestResolveStartLSN_GenuinelyFreshIsUnchanged pins the case the fresh
// branch is actually for. An empty repository on timeline 1 has no
// prior timeline to consult, and must still anchor at the slot.
func TestResolveStartLSN_GenuinelyFreshIsUnchanged(t *testing.T) {
	sp, _ := newFsRepo(t)
	lsn, note, err := resolveStartLSN(context.Background(), sp,
		walStreamOptions{deployment: "db1", pgConn: "x"}, 1, slotAt(7), nil)
	if err != nil {
		t.Fatalf("resolveStartLSN: %v", err)
	}
	if lsn != seg(7) {
		t.Errorf("resume LSN = %s, want %s (the slot's restart_lsn)", lsn, seg(7))
	}
	if note != "fresh-slot-restart-lsn" {
		t.Errorf("note = %q, want fresh-slot-restart-lsn", note)
	}
}

// TestResolveStartLSN_FreshOnALaterTimelineStaysFresh: a deployment
// whose FIRST stream happens to start on a promoted cluster has no
// archived WAL anywhere. There is nothing to skip, so the fresh
// behaviour is right even though the timeline is above 1.
func TestResolveStartLSN_FreshOnALaterTimelineStaysFresh(t *testing.T) {
	sp, _ := newFsRepo(t)
	lsn, note, err := resolveStartLSN(context.Background(), sp,
		walStreamOptions{deployment: "db1", pgConn: "x"}, 5, slotAt(7), nil)
	if err != nil {
		t.Fatalf("resolveStartLSN: %v", err)
	}
	if lsn != seg(7) || note != "fresh-slot-restart-lsn" {
		t.Errorf("got %s (%s), want %s (fresh-slot-restart-lsn) — no timeline below 5 has "+
			"any segment, so there is genuinely nothing to resume from", lsn, note, seg(7))
	}
}

// TestResolveStartLSN_PrefersTheNearestTimelineBelow guards the
// direction of the scan.
//
// TLI 1 holds segments up to 40; TLI 2 branched earlier and holds up to
// 10; the cluster is now on TLI 3. TLI 1's segments beyond TLI 2's
// branch point are DIVERGED history — written by a primary that was
// later fenced — and replaying them onto this lineage would corrupt it.
// The frontier that matters is TLI 2's.
//
// A max-across-all-timelines implementation would pick TLI 1's segment
// 41 here and pass the two tests above, so this is the one that pins
// the reasoning rather than the result.
func TestResolveStartLSN_PrefersTheNearestTimelineBelow(t *testing.T) {
	sp, _ := newFsRepo(t)
	const dep = "db1"
	for n := uint64(0); n <= 40; n++ {
		putRealSeg(t, sp, dep, 1, n)
	}
	for n := uint64(0); n <= 10; n++ {
		putRealSeg(t, sp, dep, 2, n)
	}

	lsn, note, err := resolveStartLSN(context.Background(), sp,
		walStreamOptions{deployment: dep, pgConn: "x"}, 3, slotAt(4), nil)
	if err != nil {
		t.Fatalf("resolveStartLSN: %v", err)
	}
	if want := seg(11); lsn != want {
		t.Errorf("resume LSN = %s (%s), want %s — the frontier of TLI 2, the timeline "+
			"this one branched from.\n\nTLI 1 reaches segment 41, but everything it holds "+
			"past TLI 2's branch point was written by a primary that was subsequently "+
			"fenced. Those bytes are a different history, not missing WAL.",
			lsn, note, want)
	}
}
