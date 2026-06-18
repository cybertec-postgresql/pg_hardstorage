package walsink_test

import (
	"fmt"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
)

// TestSegmentNaming_AllSizes pins the segment-naming math against PG's
// XLogFileName for every supported wal_segment_size. The invariants
// (independent of segment size, derived straight from PG's algorithm):
//
//	logID  (middle 8 hex) = LSN >> 32                      — size-INDEPENDENT
//	segLo  (last 8 hex)   = (LSN & 0xFFFFFFFF) / size
//	segNum (contiguous)   = LSN / size = logID*perLog + segLo
//
// We sweep LSNs that cross the 4 GiB log-id boundary — the case the old
// "(segNum+1)*16MiB" inventory math got wrong — for every size.
func TestSegmentNaming_AllSizes(t *testing.T) {
	sizes := []int64{
		1 << 20,  // 1 MiB  → 4096 segments/log
		2 << 20,  // 2 MiB
		16 << 20, // 16 MiB (default) → 256/log
		64 << 20, // 64 MiB → 64/log
		1 << 30,  // 1 GiB → 4/log
	}
	// LSNs to exercise per size, including past 4 GiB (logID >= 1).
	lsns := []uint64{
		0,
		1 << 20,            // 1 MiB
		16 << 20,           // 16 MiB
		64 << 20,           // 64 MiB
		1 << 30,            // 1 GiB
		1 << 32,            // 4 GiB exactly — logID rolls to 1
		(1 << 32) + 64<<20, // just past 4 GiB
		5 << 32,            // logID 5
	}
	for _, size := range sizes {
		perLog := uint64(0x100000000) / uint64(size)
		if got := walsink.SegmentsPerLog(size); got != perLog {
			t.Fatalf("SegmentsPerLog(%d)=%d, want %d", size, got, perLog)
		}
		for _, lsn := range lsns {
			segNum := lsn / uint64(size)
			wantLogID := uint32(lsn >> 32)
			wantSegLo := uint32((lsn & 0xFFFFFFFF) / uint64(size))
			wantName := fmt.Sprintf("%08X%08X%08X", uint32(7), wantLogID, wantSegLo)

			gotName := walsink.SegmentFileName(7, segNum, size)
			if gotName != wantName {
				t.Errorf("size=%d lsn=%#x: SegmentFileName=%q, want %q (logID=%d segLo=%d)",
					size, lsn, gotName, wantName, wantLogID, wantSegLo)
			}
			// Round-trip: name → (tli, segNum) at the SAME size.
			gotTLI, gotSeg, err := walsink.ParseSegmentName(gotName, size)
			if err != nil {
				t.Errorf("size=%d lsn=%#x: ParseSegmentName(%q) err: %v", size, lsn, gotName, err)
				continue
			}
			if gotTLI != 7 || gotSeg != segNum {
				t.Errorf("size=%d lsn=%#x: round-trip = (tli=%d, seg=%d), want (7, %d)",
					size, lsn, gotTLI, gotSeg, segNum)
			}
		}
	}
}

// TestSegmentNaming_KnownPGValues hardcodes a few segment names exactly
// as PG's pg_walfile_name would produce them, so a refactor of the math
// can't drift from real PG output.
func TestSegmentNaming_KnownPGValues(t *testing.T) {
	cases := []struct {
		size   int64
		segNum uint64
		want   string
	}{
		// 16 MiB default: segNum 5 → 00000001 00000000 00000005.
		{16 << 20, 5, "000000010000000000000005"},
		// 16 MiB at the log-id roll: segNum 256 → 00000001 00000001 00000000.
		{16 << 20, 256, "000000010000000100000000"},
		// 64 MiB: 64/log, segNum 2 → ...00000002; segNum 64 → log roll.
		{64 << 20, 2, "000000010000000000000002"},
		{64 << 20, 64, "000000010000000100000000"},
		// 1 MiB: 4096/log, segNum 4096 → log roll.
		{1 << 20, 10, "00000001000000000000000A"},
		{1 << 20, 4096, "000000010000000100000000"},
		// 1 GiB: 4/log, segNum 4 → log roll.
		{1 << 30, 4, "000000010000000100000000"},
	}
	for _, c := range cases {
		if got := walsink.SegmentFileName(1, c.segNum, c.size); got != c.want {
			t.Errorf("SegmentFileName(1, %d, %d) = %q, want %q", c.segNum, c.size, got, c.want)
		}
		_, gotSeg, err := walsink.ParseSegmentName(c.want, c.size)
		if err != nil || gotSeg != c.segNum {
			t.Errorf("ParseSegmentName(%q, %d) = (%d, %v), want (%d, nil)", c.want, c.size, gotSeg, err, c.segNum)
		}
	}
}
