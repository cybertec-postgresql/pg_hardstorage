package cli

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
)

// TestValidSegmentSize pins which wal_segment_size values the streamer
// accepts: every power of two in [1 MiB, 1 GiB] is valid; anything else
// (non-power-of-two, below 1 MiB, above 1 GiB) is rejected. pg_hardstorage
// now SUPPORTS non-default segment sizes — it probes the cluster's actual
// size and streams with it — so the old "refuse anything but 16 MiB"
// behaviour is gone; only impossible sizes are refused.
func TestValidSegmentSize(t *testing.T) {
	valid := []int64{1 << 20, 2 << 20, 4 << 20, 16 << 20, 64 << 20, 256 << 20, 1 << 30}
	for _, s := range valid {
		if !walsink.ValidSegmentSize(s) {
			t.Errorf("ValidSegmentSize(%d) = false, want true (a real --wal-segsize value)", s)
		}
	}
	invalid := []int64{0, -1, 1 << 19 /* 512 KiB */, 3 << 20 /* not pow2 */, (1 << 30) * 2 /* 2 GiB */, 100}
	for _, s := range invalid {
		if walsink.ValidSegmentSize(s) {
			t.Errorf("ValidSegmentSize(%d) = true, want false", s)
		}
	}
}

// TestSegmentsPerLog pins the 4 GiB / size packing PG uses, which drives
// segment naming.
func TestSegmentsPerLog(t *testing.T) {
	cases := map[int64]uint64{
		1 << 20:  4096, // 1 MiB
		16 << 20: 256,  // 16 MiB (default)
		64 << 20: 64,   // 64 MiB
		1 << 30:  4,    // 1 GiB
		0:        256,  // 0 → default
	}
	for size, want := range cases {
		if got := walsink.SegmentsPerLog(size); got != want {
			t.Errorf("SegmentsPerLog(%d) = %d, want %d", size, got, want)
		}
	}
}

func TestWalSegSizeHuman(t *testing.T) {
	cases := map[int64]string{
		1 << 20:   "1MB",
		16 << 20:  "16MB",
		64 << 20:  "64MB",
		1 << 30:   "1GB",
		1 << 10:   "1kB",
		1<<20 + 1: "1048577 bytes",
	}
	for in, want := range cases {
		if got := walSegSizeHuman(in); got != want {
			t.Errorf("walSegSizeHuman(%d) = %q, want %q", in, got, want)
		}
	}
}
