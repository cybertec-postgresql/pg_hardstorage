// Package inventory queries the WAL archive's on-repo state.
//
// Single concept today: HighestArchivedLSN — what's the highest
// LSN we have a committed WAL segment for, on a given timeline?
// This is the "lastConfirmedLSN" the leader-follow coordinator
// uses to compute Patroni-failover gap; it's also what
// `wal repair` computes manually + the future scrub /
// repository-usage callers will need.
//
// We expose it as a public package (rather than keeping a
// private helper in cli/wal.go) because the+ leader-follow
// coordinator (internal/wal/follower) needs to query it from
// outside the cli package.
package inventory

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// SegmentSize is the canonical WAL segment size: 16 MiB. PG's
// build-time configuration can change this, but every PG cluster
// pg_hardstorage targets uses the default. The walsink package
// also pins this value; if a future cluster ships with a non-
// default segment size, both this and walsink need to update
// together.
const SegmentSize uint64 = 16 * 1024 * 1024

// HighestArchivedLSN walks wal/<deployment>/<tli-hex>/ and
// returns the end LSN (exclusive) of the highest committed
// segment. found=false when no segments are present on this
// timeline.
//
// "End LSN" means: the LSN one past the last byte of the
// highest segment. That's the natural "we have WAL up to here"
// boundary the slot-continuity gap calculation compares
// against — the slot's restart_lsn after a Patroni failover
// is meaningfully ahead of this LSN iff PG produced WAL we
// missed.
//
// Cost: O(N) over segment-manifest keys on this timeline. For
// realistic retention windows (days to weeks) this is fine; a
// future indexed lookup can drop in if profiling shows it
// matters.
func HighestArchivedLSN(ctx context.Context, sp storage.StoragePlugin, deployment string, timeline uint32) (pglogrepl.LSN, bool, error) {
	maxKey, found, err := highestSegmentKey(ctx, sp, deployment, timeline)
	if err != nil || !found {
		return 0, found, err
	}
	// Read the highest segment's manifest and return its recorded
	// EndLSN. This is exact and segment-size-agnostic: the manifest
	// carries the true LSNs (and SegmentSize), so we neither hardcode
	// 16 MiB nor mis-handle the >4 GiB case where the older
	// "(segNum+1)*SegmentSize" math diverged (segNum's encoding is not
	// the same as the contiguous LSN/size index across a log-id roll).
	rc, err := sp.Get(ctx, maxKey)
	if err != nil {
		return 0, false, fmt.Errorf("inventory: read highest segment manifest %q: %w", maxKey, err)
	}
	raw, rerr := storage.ReadAllLimited(rc, storage.MaxMetadataBytes)
	_ = rc.Close()
	if rerr != nil {
		return 0, false, fmt.Errorf("inventory: read %q: %w", maxKey, rerr)
	}
	m, perr := walsink.ParseSegmentManifest(raw)
	if perr != nil {
		return 0, false, fmt.Errorf("inventory: parse %q: %w", maxKey, perr)
	}
	end, perr := pglogrepl.ParseLSN(m.EndLSN)
	if perr != nil {
		return 0, false, fmt.Errorf("inventory: parse end_lsn %q in %q: %w", m.EndLSN, maxKey, perr)
	}
	return end, true, nil
}

// FirstWALHoleInRange scans the WAL segments archived on
// (deployment, timeline) and returns the start LSN of the FIRST segment
// that is MISSING within the inclusive LSN range [fromLSN, toLSN]. It is
// a physical, repo-side completeness check — independent of the recorded
// gap state — so it catches segments lost to a pruning bug, storage
// corruption, or a manual deletion that no gap record describes.
//
// found=false (no hole) is returned when:
//   - the range is empty (toLSN < fromLSN), or
//   - the timeline has NO archived segments at all (we can't even learn
//     the geometry, and an absent archive is the caller's separate
//     concern — surfaced by target-reachability checks, not here), or
//   - every segment covering [fromLSN, toLSN] is present.
//
// It is segment-size-aware: wal_segment_size is read from an archived
// segment manifest, so it is correct for non-default sizes and across a
// >4 GiB log-id roll. toLSN is INCLUSIVE — the segment that CONTAINS it
// is required and therefore checked.
func FirstWALHoleInRange(ctx context.Context, sp storage.StoragePlugin, deployment string, timeline uint32, fromLSN, toLSN pglogrepl.LSN) (pglogrepl.LSN, bool, error) {
	if sp == nil || deployment == "" {
		return 0, false, fmt.Errorf("inventory: FirstWALHoleInRange requires sp + deployment")
	}
	if toLSN < fromLSN {
		return 0, false, nil
	}
	prefix := fmt.Sprintf("wal/%s/%08X/", deployment, timeline)

	// Memory is bounded by the RANGE asked about, not by how much WAL
	// the archive holds: one bit per segment of [fromLSN, toLSN], and
	// only for segments inside it. A years-long archive (hundreds of
	// millions of segments) used to be materialised as a name slice
	// plus a presence map — tens of GB. The segment size must be known
	// before the first name can be mapped to a bit, so the first few
	// names wait in a tiny buffer while it is learned from a manifest.
	var (
		segSize   int64
		pending   []string // names seen before segSize was fixed (<= holeScanSizeProbes)
		anySeg    bool
		fromSeg   uint64
		spanSegs  uint64
		bits      []uint64
		prepErr   error
		probesTry int
	)
	prepare := func() {
		segSize = walsink.NormSegmentSize(segSize) // default 16 MiB if unreadable
		fromSeg = uint64(fromLSN) / uint64(segSize)
		toSeg := uint64(toLSN) / uint64(segSize)
		spanSegs = toSeg - fromSeg + 1
		if spanSegs > holeScanMaxSegments {
			prepErr = fmt.Errorf("inventory: WAL hole scan over %d segments exceeds the %d-segment bound", spanSegs, holeScanMaxSegments)
			return
		}
		bits = make([]uint64, (spanSegs+63)/64)
	}
	mark := func(base string) {
		if _, segNum, err := walsink.ParseSegmentName(base, segSize); err == nil {
			if segNum >= fromSeg && segNum-fromSeg < spanSegs {
				i := segNum - fromSeg
				bits[i>>6] |= 1 << (i & 63)
			}
		}
	}
	for info, lerr := range sp.List(ctx, prefix) {
		if lerr != nil {
			return 0, false, lerr
		}
		if cerr := ctx.Err(); cerr != nil {
			return 0, false, cerr
		}
		key := info.Key
		if !strings.HasSuffix(key, ".json") || segmentKeyIsStagingTemp(key) {
			continue
		}
		base := key[len(prefix) : len(key)-len(".json")]
		if len(base) != 24 {
			continue
		}
		anySeg = true
		if bits != nil {
			mark(base)
			continue
		}
		pending = append(pending, base)
		if segSize == 0 {
			// Learn the cluster's segment size from the first readable manifest.
			probesTry++
			if rc, gerr := sp.Get(ctx, key); gerr == nil {
				raw, rerr := storage.ReadAllLimited(rc, storage.MaxMetadataBytes)
				_ = rc.Close()
				if rerr == nil {
					if m, perr := walsink.ParseSegmentManifest(raw); perr == nil && walsink.ValidSegmentSize(m.SegmentSize) {
						segSize = m.SegmentSize
					}
				}
			}
		}
		if segSize != 0 || probesTry >= holeScanSizeProbes {
			prepare()
			if prepErr != nil {
				return 0, false, prepErr
			}
			for _, n := range pending {
				mark(n)
			}
			pending = nil
		}
	}
	if !anySeg {
		return 0, false, nil // no archived WAL on this timeline
	}
	if bits == nil { // fewer names than probes, none readable
		prepare()
		if prepErr != nil {
			return 0, false, prepErr
		}
		for _, n := range pending {
			mark(n)
		}
	}
	for i := uint64(0); i < spanSegs; i++ {
		if bits[i>>6]&(1<<(i&63)) == 0 {
			return pglogrepl.LSN((fromSeg + i) * uint64(segSize)), true, nil
		}
	}
	return 0, false, nil
}

// holeScanMaxSegments bounds the presence bitmap of FirstWALHoleInRange
// (one bit per segment): 1<<29 segments is a 64 MiB bitmap covering 8 PiB
// of 16 MiB WAL. A wider range (an absurd --to-lsn) returns an error,
// which every caller already treats as "probe unavailable".
var holeScanMaxSegments uint64 = 1 << 29

// holeScanSizeProbes is how many segment manifests are tried when
// learning the cluster's segment size before falling back to 16 MiB.
const holeScanSizeProbes = 16

// highestSegmentKey returns the repo key of the highest committed
// segment manifest on the timeline. Comparison is by the monotonic
// (log_id, seg_in_log) ordering the 24-char name encodes, which is
// segment-size-INDEPENDENT: a higher log_id — or a higher seg_in_log
// within the same log_id — is always a later segment, whatever the
// segment size (seg_in_log is an 8-hex field, always < 2^32). So we can
// pick the highest name without knowing the segment size, then read its
// manifest for the true LSN.
func highestSegmentKey(ctx context.Context, sp storage.StoragePlugin, deployment string, timeline uint32) (string, bool, error) {
	if sp == nil {
		return "", false, fmt.Errorf("inventory: nil StoragePlugin")
	}
	if deployment == "" {
		return "", false, fmt.Errorf("inventory: empty deployment")
	}
	prefix := fmt.Sprintf("wal/%s/%08X/", deployment, timeline)
	var (
		maxSeg uint64
		maxKey string
		any    bool
	)
	for info, lerr := range sp.List(ctx, prefix) {
		if lerr != nil {
			return "", false, lerr
		}
		// Cooperative cancellation: long retention windows can
		// produce hundreds of thousands of segments; the
		// operator's Ctrl-C should reach this loop without
		// waiting for the underlying List to complete.
		if cerr := ctx.Err(); cerr != nil {
			return "", false, cerr
		}
		const wantSuffix = ".json"
		key := info.Key
		if !strings.HasSuffix(key, wantSuffix) {
			continue
		}
		// Skip in-flight tmp files (foo.json.tmp.*).
		if segmentKeyIsStagingTemp(key) {
			continue
		}
		base := key[len(prefix) : len(key)-len(wantSuffix)]
		if len(base) != 24 {
			// Defensive: ignore anything that doesn't look
			// like a canonical 24-hex-char segment name. Lets
			// the repo grow auxiliary metadata files alongside
			// without breaking the walk.
			continue
		}
		segNum, ok := parseSegmentNumber(base)
		if !ok {
			continue
		}
		if !any || segNum > maxSeg {
			maxSeg = segNum
			maxKey = key
			any = true
		}
	}
	return maxKey, any, nil
}

// parseSegmentNumber decodes a 24-char hex segment name into a
// MONOTONIC SORT KEY — the LogID:LogSeg concatenation (lower 16 chars
// as a 64-bit value). This is NOT the contiguous absolute segment
// number (that would be LogID*segmentsPerLog + LogSeg, which needs the
// segment size); we only use it to pick the HIGHEST segment name, for
// which the concatenation is order-preserving regardless of segment
// size (LogSeg is an 8-hex field, always < 2^32, so a higher LogID
// always dominates). The true LSN comes from that segment's manifest.
//
// The TLI prefix is redundant with the directory name, so we
// don't need to extract it here; a future "is this segment in
// the right TLI dir" sanity check could re-derive it.
func parseSegmentNumber(s string) (uint64, bool) {
	if len(s) != 24 {
		return 0, false
	}
	var n uint64
	for i := 8; i < 24; i++ { // skip TLI prefix
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			n = n<<4 | uint64(c-'0')
		case c >= 'A' && c <= 'F':
			n = n<<4 | uint64(c-'A'+10)
		case c >= 'a' && c <= 'f':
			n = n<<4 | uint64(c-'a'+10)
		default:
			return 0, false
		}
	}
	return n, true
}
