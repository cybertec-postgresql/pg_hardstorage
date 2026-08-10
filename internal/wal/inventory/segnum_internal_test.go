package inventory

// segnum_internal_test.go — parseSegmentNumber is the sort key that
// picks the HIGHEST archived WAL segment, which anchors the archive
// frontier used across the failover-gap machinery (#17-22). It is
// size-agnostic on purpose: it does NOT compute the contiguous
// segment number (that needs the segment size and lives in
// walsink.ParseSegmentName, which is separately tested across the
// 4 GiB log-id roll), it only produces an ORDER-PRESERVING key. This
// pins that ordering property — especially the log-id roll, where a
// naive scheme picks the wrong "highest" and the frontier jumps
// backward, which would make gap detection lie.

import (
	"fmt"
	"testing"
)

// name builds a canonical 24-hex segment name from (tli, logID, segLo).
func name(tli, logID, segLo uint32) string {
	return fmt.Sprintf("%08X%08X%08X", tli, logID, segLo)
}

func TestParseSegmentNumber_OrderingAcrossLogIdRoll(t *testing.T) {
	// The critical pair: the LAST segment of log N must sort BELOW the
	// FIRST segment of log N+1, even at the maximal seg-in-log value.
	// If parseSegmentNumber ever stopped being order-preserving here,
	// highestSegmentKey would pick a stale segment and the archive
	// frontier would move backward.
	pairs := []struct{ lo, hi string }{
		{name(1, 0, 0xFFFFFFFF), name(1, 1, 0x00000000)},          // log 0 max < log 1 min
		{name(1, 5, 0x000000FF), name(1, 5, 0x00000100)},          // same log, seg order
		{name(1, 0x0000FFFF, 0xFFFFFFFF), name(1, 0x00010000, 0)}, // higher log-id roll
		{name(7, 3, 10), name(7, 3, 11)},                          // trivial adjacent
	}
	for _, p := range pairs {
		lo, ok1 := parseSegmentNumber(p.lo)
		hi, ok2 := parseSegmentNumber(p.hi)
		if !ok1 || !ok2 {
			t.Fatalf("parse failed: %q(%v) %q(%v)", p.lo, ok1, p.hi, ok2)
		}
		if !(lo < hi) {
			t.Errorf("ordering broken: parseSegmentNumber(%q)=%d NOT < parseSegmentNumber(%q)=%d "+
				"— the archive frontier would select the wrong highest segment and move "+
				"BACKWARD across a log-id roll, making gap detection lie", p.lo, lo, p.hi, hi)
		}
	}
}

func TestParseSegmentNumber_RejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		"", "short", "00000001000000000000000G", // non-hex
		"00000001000000000000000",   // 23 chars
		"000000010000000000000000A", // 25 chars
	} {
		if _, ok := parseSegmentNumber(bad); ok {
			t.Errorf("parseSegmentNumber(%q) accepted a malformed name", bad)
		}
	}
}

func FuzzParseSegmentNumber(f *testing.F) {
	f.Add("000000010000000000000005")
	f.Add("00000001FFFFFFFF00000000")
	f.Add("garbage")
	f.Fuzz(func(t *testing.T, s string) {
		n, ok := parseSegmentNumber(s) // must never panic
		// Only 24-char all-hex names are accepted, and acceptance must
		// be deterministic under a second call.
		if n2, ok2 := parseSegmentNumber(s); ok != ok2 || n != n2 {
			t.Fatalf("non-deterministic parse of %q", s)
		}
	})
}
