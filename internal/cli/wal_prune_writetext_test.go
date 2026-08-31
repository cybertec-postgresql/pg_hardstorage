package cli

// `wal prune` used to render its byte figure as "Bytes deleted", which
// was wrong in three independent directions:
//
//   - the command deletes segment MANIFESTS; the chunks stay until
//     `repo gc` runs, so immediately after a prune every one of those
//     bytes is still on disk;
//   - the figure sums ChunkRef.Len, the PLAINTEXT size, while chunks
//     are stored compressed and enveloped;
//   - chunks are content-addressed and deduplicated, so one still
//     referenced by a live segment is never removed at all.
//
// An operator reading "Bytes deleted: 12.4 GiB" concludes the space is
// back and can decide not to expand a volume on the strength of it.

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func renderPrune(t *testing.T, b walPruneBody) string {
	t.Helper()
	var sb strings.Builder
	if err := b.WriteText(&sb); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return sb.String()
}

func TestWalPruneWriteText_DoesNotClaimBytesWereDeleted(t *testing.T) {
	b := walPruneBody{WALPruneResult: repo.WALPruneResult{
		Deployment: "db1", FrontierBackupID: "db1.full.001", FrontierLSN: "0/3000000", SegmentsConsidered: 100, SegmentsDeleted: 40,
		SegmentsKept: 60, BytesDeleted: 12 << 30,
	}}
	out := renderPrune(t, b)

	if strings.Contains(out, "Bytes deleted") {
		t.Errorf("output still claims the bytes were deleted; this command removes manifests "+
			"only and the chunks stay until `repo gc`:\n%s", out)
	}
	if !strings.Contains(out, "repo gc") {
		t.Errorf("the operator must be told what actually reclaims the space:\n%s", out)
	}
	if !strings.Contains(out, "still on disk") {
		t.Errorf("the output must say the bytes have not been freed yet:\n%s", out)
	}
	for _, want := range []string{"logical", "deduplicated"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q — without it the figure reads as a real "+
				"on-disk saving:\n%s", want, out)
		}
	}
	// The number itself must still be shown; the fix is the framing.
	if !strings.Contains(out, "12") {
		t.Errorf("the byte figure disappeared from the output:\n%s", out)
	}
}

// The counters that ARE accurate must survive the rewording.
func TestWalPruneWriteText_SegmentCountersStillReported(t *testing.T) {
	b := walPruneBody{WALPruneResult: repo.WALPruneResult{
		Deployment: "db1", FrontierBackupID: "db1.full.001", FrontierLSN: "0/3000000", SegmentsConsidered: 100, SegmentsDeleted: 40,
		SegmentsKept: 59, SegmentsFailed: 1,
	}}
	out := renderPrune(t, b)
	for _, want := range []string{"100 considered", "40 deleted", "59 kept", "1 failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("segment line lost %q:\n%s", want, out)
		}
	}
}
