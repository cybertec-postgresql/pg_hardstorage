package cli

// wal_stream_resume_walk_test.go — the composition, not the pieces.
//
// The pieces have unit tests: resolveStartLSN picks a resume point,
// timeline.Containing picks a timeline, resolveStreamTimeline glues
// them. Each was individually "obviously right" at two separate
// points in this fix, and the composition livelocked anyway — a
// streamer that reconnected forever, re-requesting the same
// sub-segment tail and committing nothing, until its own no-progress
// backstop stopped it.
//
// What makes that failure invisible to unit tests is that no single
// call is wrong. The loop is wrong. So this drives the REAL resolve
// functions over a REAL repository through the actual reconnect
// cycle — resolve, stream, archive whole segments, resolve again —
// and asserts the property that matters: every reconnect makes
// progress, and the walk reaches the live timeline.
//
// The chaos gate does exercise this path, but only when the timing
// happens to put the streamer far enough behind: the same seed
// reached timeline 29 in one run and timeline 4 in another. A gate
// that finds the bug sometimes is not a regression test.

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	waltimeline "github.com/cybertec-postgresql/pg_hardstorage/internal/wal/timeline"
)

func walkSP(t *testing.T) storage.StoragePlugin {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

// archiveSegment plants the manifest the walsink writes when it
// commits a whole segment, under the timeline it was streamed on.
func archiveSegment(t *testing.T, sp storage.StoragePlugin, dep string, tli uint32, segNum uint64, segSize int64) {
	t.Helper()
	start := pglogrepl.LSN(segNum * uint64(segSize))
	m := &walsink.SegmentManifest{
		Schema:           walsink.Schema,
		Deployment:       dep,
		SystemIdentifier: "7000000000000000001",
		Timeline:         tli,
		SegmentNumber:    segNum,
		SegmentName:      segName(tli, segNum, segSize),
		StartLSN:         start.String(),
		EndLSN:           (start + pglogrepl.LSN(segSize)).String(),
		SegmentSize:      segSize,
	}
	raw, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	key := walsink.SegmentPath(dep, tli, m.SegmentName)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(raw),
		storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
		t.Fatal(err)
	}
}

// segName builds PostgreSQL's 24-char WAL file name the way
// XLogFileName does: timeline, then the segment number split across
// the log-id boundary (0x100000000 / segment size segments per id).
func segName(tli uint32, segNum uint64, segSize int64) string {
	perID := uint64(0x100000000) / uint64(segSize)
	return fmt.Sprintf("%08X%08X%08X", tli, segNum/perID, segNum%perID)
}

// TestResumeWalk_ReachesTheLiveTimelineAndAlwaysCommits drives the
// reconnect loop the way runWalStream does and asserts it terminates.
//
// Geometry is the chaos gate's: the archive holds timeline 27 up to
// segment A1, the cluster has promoted twice (27 ended inside A3, 28
// inside A6) and is live on 29. Before the fix the first resolve
// asked timeline 29 for an LSN in A1 — a segment that timeline can
// never have — and the stream stopped for good.
func TestResumeWalk_ReachesTheLiveTimelineAndAlwaysCommits(t *testing.T) {
	const (
		dep     = "db1"
		live    = uint32(29)
		segSize = int64(16 << 20)
	)
	ctx := context.Background()
	sp := walkSP(t)

	// The ancestry, stored the way `wal stream` captures it.
	history := "27\t0/A3079D40\tno recovery target specified\n" +
		"28\t0/A60B6668\tno recovery target specified\n"
	if err := waltimeline.New(sp).Put(ctx, dep, live, []byte(history)); err != nil {
		t.Fatal(err)
	}
	// Where PG can actually serve from, per timeline: the switchpoint
	// for an ancestor, and "plenty more" for the live one.
	switchpoint := map[uint32]pglogrepl.LSN{
		27: 0xA3079D40,
		28: 0xA60B6668,
		29: 0xA9000000,
	}
	// The floor each timeline can actually SERVE from. A timeline's
	// segment files begin at the segment CONTAINING its fork point —
	// PostgreSQL copies the parent's last partial segment under the
	// new timeline's name at promotion — and nothing below that has
	// ever existed under this timeline's name. Asking for one is the
	// original bug, and PG cannot tell it apart from a recycled
	// segment: it answers "already been removed" and the streamer
	// stops for good.
	servableFrom := map[uint32]pglogrepl.LSN{
		27: 0,
		28: 0xA3079D40 / pglogrepl.LSN(segSize) * pglogrepl.LSN(segSize),
		29: 0xA60B6668 / pglogrepl.LSN(segSize) * pglogrepl.LSN(segSize),
	}

	// What the archive already holds: timeline 27, segment A1.
	archiveSegment(t, sp, dep, 27, 0xA1, segSize)

	opts := walStreamOptions{deployment: dep, segmentSize: segSize}
	slot := &replication.SlotInfo{Name: "s", Type: "physical", RestartLSN: "0/A60B6668"}
	store := waltimeline.New(sp)

	seen := map[string]bool{}
	var reachedLive bool
	for attempt := 1; attempt <= 12; attempt++ {
		// --- exactly what streamAttempt does, in order ---
		startLSN, note, err := resolveStartLSN(ctx, sp, opts, live, slot, func(*output.Event) {})
		if err != nil {
			t.Fatalf("attempt %d: resolveStartLSN: %v", attempt, err)
		}
		streamTLI := resolveStreamTimeline(ctx, store, dep, live, startLSN, segSize, nil)

		key := fmt.Sprintf("%s@%d", startLSN, streamTLI)
		if seen[key] {
			t.Fatalf("attempt %d: the walk revisited %s on timeline %d — this is the livelock "+
				"(resume_strategy=%q)", attempt, startLSN, streamTLI, note)
		}
		seen[key] = true

		// --- simulate PG + the walsink ---
		// PG serves from startLSN up to where this timeline ends.
		// The sink commits WHOLE segments only.
		if startLSN < servableFrom[streamTLI] {
			t.Fatalf("attempt %d: opened timeline %d at %s, below the %s floor where its "+
				"segment files begin — PG would look for %s, a file that has never existed, "+
				"and refuse permanently with \"requested WAL segment ... has already been "+
				"removed\". This is the original bug.",
				attempt, streamTLI, startLSN, servableFrom[streamTLI],
				segName(streamTLI, uint64(startLSN)/uint64(segSize), segSize))
		}
		end := switchpoint[streamTLI]
		if end <= startLSN {
			t.Fatalf("attempt %d: opened timeline %d at %s, which is at or past its end %s — "+
				"PG would CopyDone immediately and nothing would commit",
				attempt, streamTLI, startLSN, end)
		}
		committed := 0
		for seg := uint64(startLSN) / uint64(segSize); (seg+1)*uint64(segSize) <= uint64(end); seg++ {
			archiveSegment(t, sp, dep, streamTLI, seg, segSize)
			committed++
		}
		if committed == 0 {
			t.Fatalf("attempt %d: opened timeline %d at %s but it has no whole segment before "+
				"its end %s — the stream commits nothing and the next attempt resolves "+
				"identically. This is the livelock the whole-segment rule prevents.",
				attempt, streamTLI, startLSN, end)
		}

		if streamTLI == live {
			reachedLive = true
			break
		}
	}
	if !reachedLive {
		t.Fatalf("the walk never reached the live timeline %d in 12 reconnects", live)
	}
}

// The ordinary case must be untouched: a caught-up streamer on a
// cluster that never failed over resolves to its own timeline and
// never consults an ancestor.
func TestResumeWalk_NoFailoverIsUnchanged(t *testing.T) {
	const (
		dep     = "db1"
		segSize = int64(16 << 20)
	)
	ctx := context.Background()
	sp := walkSP(t)
	archiveSegment(t, sp, dep, 1, 0x10, segSize)

	opts := walStreamOptions{deployment: dep, segmentSize: segSize}
	slot := &replication.SlotInfo{Name: "s", Type: "physical", RestartLSN: "0/10000000"}

	startLSN, note, err := resolveStartLSN(ctx, sp, opts, 1, slot, func(*output.Event) {})
	if err != nil {
		t.Fatalf("resolveStartLSN: %v", err)
	}
	if note != "resume-from-repo" {
		t.Errorf("resume_strategy = %q, want resume-from-repo", note)
	}
	if got := resolveStreamTimeline(ctx, waltimeline.New(sp), dep, 1, startLSN, segSize, nil); got != 1 {
		t.Fatalf("stream timeline = %d, want 1", got)
	}
}
