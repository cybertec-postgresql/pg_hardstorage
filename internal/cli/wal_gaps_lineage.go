package cli

import (
	"context"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// timelineForkLookup answers "in which segment does newTLI's lineage
// leave prevTLI?" — the segment holding the switchpoint recorded in
// newTLI's history file. ok=false when that is unknown.
type timelineForkLookup func(newTLI, prevTLI uint32) (forkSeg uint64, ok bool)

// findLineageGaps is findGaps plus the one hole segment numbers alone
// cannot see: a new timeline that starts PAST its fork.
//
// findGaps treats a timeline change as contiguous whenever the new
// timeline's first segment lands at or before the old one's last + 1,
// and treats the old timeline running past the new one's start as
// harmless overlap. Both are right only when the new timeline begins at
// its fork. When the old timeline was archived past the fork — the old
// primary kept going before it was fenced — and the new timeline was
// then archived from the OLD frontier rather than from the fork (the
// cross-timeline resume bug), the new timeline's segments from the fork
// to its first archived one exist nowhere: the old timeline's copies
// past the fork are diverged history, not the same WAL. Numbers read
// "10 then 11"; the lineage actually has a hole at 8..10.
//
// So at every timeline change whose fork is known, the new timeline
// must be covered from the fork segment. When the fork is unknown (no
// history captured, unreadable) this is exactly findGaps.
func findLineageGaps(segs []walSegment, forks timelineForkLookup) []walGap {
	gaps := findGaps(segs)
	if forks == nil {
		return gaps
	}
	for i := 1; i < len(segs); i++ {
		prev, curr := segs[i-1], segs[i]
		if prev.Timeline == curr.Timeline {
			continue
		}
		forkSeg, ok := forks(curr.Timeline, prev.Timeline)
		if !ok {
			continue
		}
		// forkSeg > prev.SegmentNumber is findGaps' territory: the old
		// timeline stopped short of the fork, and any hole shows up in the
		// plain arithmetic (prev+1 .. curr-1) already.
		if forkSeg > prev.SegmentNumber || curr.SegmentNumber <= forkSeg {
			continue
		}
		gaps = append(gaps, walGap{
			Timeline:     curr.Timeline,
			StartSegment: forkSeg,
			EndSegment:   curr.SegmentNumber - 1,
			MissingCount: curr.SegmentNumber - forkSeg,
		})
	}
	return gaps
}

// repoTimelineForks builds a timelineForkLookup from the history files
// captured in the repo's timeline store. segSize comes from the scanned
// segments (every walSegment spans exactly one segment); with no
// segments there is nothing to check and the lookup is nil.
func repoTimelineForks(ctx context.Context, sp storage.StoragePlugin, deployment string, segs []walSegment) timelineForkLookup {
	if len(segs) == 0 {
		return nil
	}
	start, serr := pglogrepl.ParseLSN(segs[0].StartLSN)
	end, eerr := pglogrepl.ParseLSN(segs[0].EndLSN)
	if serr != nil || eerr != nil || end <= start {
		return nil
	}
	segSize := int64(end - start)
	return func(newTLI, prevTLI uint32) (uint64, bool) {
		lsn, ok := lineageForkSegmentStart(ctx, sp, deployment, newTLI, prevTLI, segSize)
		if !ok {
			return 0, false
		}
		return uint64(lsn) / uint64(segSize), true
	}
}
