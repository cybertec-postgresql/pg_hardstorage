package walsink_test

// SegmentsPerLog is 0x1_0000_0000 / segment_size — how many WAL
// segments PG packs into one log-id grouping. For any size PG can
// actually have (a power of two from 1 MiB to 1 GiB) that is between 4
// and 4096.
//
// The value also arrives from OUTSIDE this process: cli/wal.go passes
// a stored Manifest.SegmentSize straight through. A corrupt or
// hand-edited manifest declaring more than 4 GiB made SegmentsPerLog
// return 0, and SegmentFileName then evaluated `segNum % 0` — so
// `wal list` and `wal verify` died with "integer divide by zero" on
// exactly the corrupt manifest they exist to inspect. Same class as any
// other crash-on-malformed-input: the tool that reports corruption must
// survive it.

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
)

// Every size PG can be initialised with, plus the boundaries.
var realSegmentSizes = []int64{
	1 << 20, 2 << 20, 4 << 20, 8 << 20, 16 << 20, 32 << 20,
	64 << 20, 128 << 20, 256 << 20, 512 << 20, 1 << 30,
}

func TestSegmentsPerLog_NeverZero(t *testing.T) {
	// Sizes no PG produces, including the ones that used to divide to
	// zero. Nothing here may return 0 — every caller divides by it.
	for _, sz := range []int64{
		0, -1, 1, 3, 4 << 30, 8 << 30, 1 << 40, 1<<62 + 1,
	} {
		if got := walsink.SegmentsPerLog(sz); got == 0 {
			t.Errorf("SegmentsPerLog(%d) = 0 — every caller divides or mods by this, so a "+
				"zero is an integer-divide-by-zero panic in SegmentFileName", sz)
		}
	}
}

// The regression proper.
func TestSegmentFileName_DoesNotPanicOnAnOversizedSegmentSize(t *testing.T) {
	for _, sz := range []int64{4 << 30, 8 << 30, 1 << 40} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("SegmentFileName(1, 5, %d) panicked: %v — a corrupt manifest's "+
						"segment_size must not crash the command reading it", sz, r)
				}
			}()
			if got := walsink.SegmentFileName(1, 5, sz); len(got) != 24 {
				t.Errorf("SegmentFileName(1, 5, %d) = %q, want a 24-char name", sz, got)
			}
		}()
	}
}

// The real contract: name and number are inverses for every size PG can
// have. These are storage KEYS for WAL — a disagreement writes segments
// where the reader will not look for them.
func TestSegmentName_RoundTripsForEveryRealSegmentSize(t *testing.T) {
	segNums := []uint64{
		0, 1, 2, 3, 4, 255, 256, 257, 4095, 4096, 4097,
		65535, 65536, 1 << 20, 1<<32 - 1, 1 << 32,
	}
	for _, sz := range realSegmentSizes {
		perLog := walsink.SegmentsPerLog(sz)
		for _, tli := range []uint32{1, 2, 0xFFFFFFFF} {
			for _, n := range segNums {
				// Above the representable range the log id would need
				// more than 32 bits; PG cannot name those either.
				if n/perLog > 0xFFFFFFFF {
					continue
				}
				name := walsink.SegmentFileName(tli, n, sz)
				if len(name) != 24 {
					t.Fatalf("size=%d tli=%d seg=%d: name %q is not 24 chars", sz, tli, n, name)
				}
				gotTLI, gotNum, err := walsink.ParseSegmentName(name, sz)
				if err != nil {
					t.Fatalf("size=%d: ParseSegmentName(%q) = %v", sz, name, err)
				}
				if gotTLI != tli || gotNum != n {
					t.Fatalf("size=%d: round trip of (tli=%d, seg=%d) via %q gave (tli=%d, seg=%d) "+
						"— segment names are WAL storage keys, so a mismatch writes segments "+
						"where the reader will not look", sz, tli, n, name, gotTLI, gotNum)
				}
			}
		}
	}
}

// Distinct segments must get distinct names, at every size. A collision
// means one segment silently overwrites another in the archive.
func TestSegmentFileName_IsInjectiveWithinATimeline(t *testing.T) {
	for _, sz := range realSegmentSizes {
		seen := make(map[string]uint64, 5000)
		for n := uint64(0); n < 5000; n++ {
			name := walsink.SegmentFileName(1, n, sz)
			if prev, dup := seen[name]; dup {
				t.Fatalf("size=%d: segments %d and %d both map to %q — one would silently "+
					"overwrite the other in the archive", sz, prev, n, name)
			}
			seen[name] = n
		}
	}
}

func TestSegmentsPerLog_KnownValues(t *testing.T) {
	for _, tc := range []struct {
		size int64
		want uint64
	}{
		{1 << 20, 4096}, {16 << 20, 256}, {64 << 20, 64}, {1 << 30, 4},
		{0, 256}, // unset resolves to the 16 MiB default
	} {
		if got := walsink.SegmentsPerLog(tc.size); got != tc.want {
			t.Errorf("SegmentsPerLog(%d) = %d, want %d", tc.size, got, tc.want)
		}
	}
}
