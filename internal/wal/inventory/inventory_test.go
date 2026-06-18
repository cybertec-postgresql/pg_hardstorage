package inventory_test

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/inventory"
)

// newSP builds a temp file:// storage plugin.
func newSP(t *testing.T) storage.StoragePlugin {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

// putSegmentManifest plants a real segment manifest at
// wal/<deployment>/<tli>/<segname>.json with the StartLSN/EndLSN the
// streamer would record (default 16 MiB segments). HighestArchivedLSN
// reads the EndLSN back, so the body must be a valid manifest now.
func putSegmentManifest(t *testing.T, sp storage.StoragePlugin, deployment string, tli uint32, segName string) {
	t.Helper()
	_, segNum, perr := walsink.ParseSegmentName(segName, walsink.SegmentSize)
	if perr != nil {
		t.Fatalf("putSegmentManifest: %q is not a valid segment name: %v", segName, perr)
	}
	start := pglogrepl.LSN(uint64(segNum) * uint64(walsink.SegmentSize))
	end := start + pglogrepl.LSN(walsink.SegmentSize)
	m := &walsink.SegmentManifest{
		Schema:           walsink.Schema,
		Deployment:       deployment,
		SystemIdentifier: "7000000000000000001",
		Timeline:         tli,
		SegmentNumber:    segNum,
		SegmentName:      segName,
		StartLSN:         start.String(),
		EndLSN:           end.String(),
		SegmentSize:      walsink.SegmentSize,
	}
	raw, err := m.MarshalToBytes()
	if err != nil {
		t.Fatalf("marshal segment manifest: %v", err)
	}
	key := fmt.Sprintf("wal/%s/%08X/%s.json", deployment, tli, segName)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(raw),
		storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
		t.Fatal(err)
	}
}

// TestHighestArchivedLSN_NoSegments: empty timeline returns
// found=false, no error. Boundary case for the leader-follow
// coordinator's first-time bootstrap.
func TestHighestArchivedLSN_NoSegments(t *testing.T) {
	sp := newSP(t)
	lsn, found, err := inventory.HighestArchivedLSN(context.Background(), sp, "db1", 1)
	if err != nil {
		t.Fatalf("HighestArchivedLSN: %v", err)
	}
	if found {
		t.Errorf("found = true, want false (no segments)")
	}
	if lsn != 0 {
		t.Errorf("lsn = %v, want 0", lsn)
	}
}

// TestHighestArchivedLSN_SingleSegment: one committed segment;
// the returned LSN is one segment's worth past zero
// (16 MiB × (segNum+1)).
func TestHighestArchivedLSN_SingleSegment(t *testing.T) {
	sp := newSP(t)
	// Segment name format: 8-char TLI + 8-char LogID + 8-char LogSeg.
	// TLI=00000001, LogID=00000000, LogSeg=00000000 → segNum = 0,
	// end LSN = (0+1) * 16 MiB = 16 MiB = 0x1000000.
	putSegmentManifest(t, sp, "db1", 1, "000000010000000000000000")

	lsn, found, err := inventory.HighestArchivedLSN(context.Background(), sp, "db1", 1)
	if err != nil {
		t.Fatalf("HighestArchivedLSN: %v", err)
	}
	if !found {
		t.Fatal("found = false; want true")
	}
	if uint64(lsn) != inventory.SegmentSize {
		t.Errorf("lsn = %x, want %x", uint64(lsn), inventory.SegmentSize)
	}
}

// TestHighestArchivedLSN_TakesMaxAcrossSegments: with multiple
// committed segments, the highest one wins. Pin the arithmetic:
// segNum=5 → end LSN = 6 * 16 MiB = 0x6000000.
func TestHighestArchivedLSN_TakesMaxAcrossSegments(t *testing.T) {
	sp := newSP(t)
	for _, name := range []string{
		"000000010000000000000000",
		"000000010000000000000001",
		"000000010000000000000005",
		"000000010000000000000003",
	} {
		putSegmentManifest(t, sp, "db1", 1, name)
	}
	lsn, found, err := inventory.HighestArchivedLSN(context.Background(), sp, "db1", 1)
	if err != nil {
		t.Fatalf("HighestArchivedLSN: %v", err)
	}
	if !found {
		t.Fatal("found = false")
	}
	wantLSN := pglogrepl.LSN(6 * inventory.SegmentSize)
	if lsn != wantLSN {
		t.Errorf("lsn = %v, want %v", lsn, wantLSN)
	}
}

// TestHighestArchivedLSN_IgnoresInFlightTmp: a `.json.tmp.<rand>`
// in-flight upload must NOT count as a committed segment.
func TestHighestArchivedLSN_IgnoresInFlightTmp(t *testing.T) {
	sp := newSP(t)
	// Real committed segment.
	putSegmentManifest(t, sp, "db1", 1, "000000010000000000000000")
	// In-flight tmp at a much higher seg number.
	tmpKey := "wal/db1/00000001/000000010000000000000099.json.tmp.deadbeef"
	if _, err := sp.Put(context.Background(), tmpKey, strings.NewReader(`{}`), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	lsn, found, err := inventory.HighestArchivedLSN(context.Background(), sp, "db1", 1)
	if err != nil {
		t.Fatalf("HighestArchivedLSN: %v", err)
	}
	if !found {
		t.Fatal("found = false")
	}
	// Must reflect ONLY the committed segment; tmp is ignored.
	if uint64(lsn) != inventory.SegmentSize {
		t.Errorf("lsn = %x, want %x (tmp file should not count)", uint64(lsn), inventory.SegmentSize)
	}
}

// TestHighestArchivedLSN_IgnoresMalformedNames: defensive
// against repo growing auxiliary metadata files alongside the
// canonical 24-hex-char segment names.
func TestHighestArchivedLSN_IgnoresMalformedNames(t *testing.T) {
	sp := newSP(t)
	putSegmentManifest(t, sp, "db1", 1, "000000010000000000000002")
	// Plant a non-segment-shaped file at the same prefix.
	bogusKey := "wal/db1/00000001/notes.json"
	if _, err := sp.Put(context.Background(), bogusKey, strings.NewReader(`{}`), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	lsn, found, err := inventory.HighestArchivedLSN(context.Background(), sp, "db1", 1)
	if err != nil {
		t.Fatalf("HighestArchivedLSN: %v", err)
	}
	if !found {
		t.Fatal("found = false")
	}
	wantLSN := pglogrepl.LSN(3 * inventory.SegmentSize)
	if lsn != wantLSN {
		t.Errorf("lsn = %v, want %v (malformed name should be ignored)", lsn, wantLSN)
	}
}

// TestHighestArchivedLSN_DeploymentScoped: segments under a
// different deployment must NOT contribute to the result.
func TestHighestArchivedLSN_DeploymentScoped(t *testing.T) {
	sp := newSP(t)
	putSegmentManifest(t, sp, "db1", 1, "000000010000000000000003")
	putSegmentManifest(t, sp, "db2", 1, "000000010000000000000099") // higher but other deployment

	lsn, found, err := inventory.HighestArchivedLSN(context.Background(), sp, "db1", 1)
	if err != nil {
		t.Fatalf("HighestArchivedLSN: %v", err)
	}
	if !found {
		t.Fatal("found = false")
	}
	wantLSN := pglogrepl.LSN(4 * inventory.SegmentSize)
	if lsn != wantLSN {
		t.Errorf("lsn = %v, want %v (db2 should not leak in)", lsn, wantLSN)
	}
}

// TestHighestArchivedLSN_TimelineScoped: segments on a different
// TLI must NOT contribute. Required by the leader-follow
// coordinator's gap calculation, which queries by the new
// leader's reported TLI.
func TestHighestArchivedLSN_TimelineScoped(t *testing.T) {
	sp := newSP(t)
	putSegmentManifest(t, sp, "db1", 1, "000000010000000000000005")
	putSegmentManifest(t, sp, "db1", 2, "000000020000000000000099") // different TLI

	lsn, _, err := inventory.HighestArchivedLSN(context.Background(), sp, "db1", 1)
	if err != nil {
		t.Fatal(err)
	}
	wantLSN := pglogrepl.LSN(6 * inventory.SegmentSize)
	if lsn != wantLSN {
		t.Errorf("TLI 1 lsn = %v, want %v (TLI 2 should not leak)", lsn, wantLSN)
	}
}

// TestHighestArchivedLSN_RejectsBadInput: nil sp / empty
// deployment surface clean errors.
func TestHighestArchivedLSN_RejectsBadInput(t *testing.T) {
	_, _, err := inventory.HighestArchivedLSN(context.Background(), nil, "db1", 1)
	if err == nil {
		t.Error("nil sp should error")
	}
	sp := newSP(t)
	_, _, err = inventory.HighestArchivedLSN(context.Background(), sp, "", 1)
	if err == nil {
		t.Error("empty deployment should error")
	}
}

// TestSegmentSize_Constant: regression guard. The walsink
// package and the rest of the system pin SegmentSize to 16 MiB.
// A future PG with a non-default WAL segment size would need
// every callsite updated; pin the value here so a typo is caught.
func TestSegmentSize_Constant(t *testing.T) {
	if inventory.SegmentSize != 16*1024*1024 {
		t.Errorf("SegmentSize = %d, want %d (16 MiB)",
			inventory.SegmentSize, 16*1024*1024)
	}
}

// TestFirstWALHoleInRange pins the physical missing-segment detector: a
// gap between archived segments inside the queried LSN range is found
// (with the correct hole LSN); a contiguous range and an empty timeline
// report no hole.
func TestFirstWALHoleInRange(t *testing.T) {
	sp := newSP(t)
	ctx := context.Background()
	const seg = walsink.SegmentSize // 16 MiB
	// Archive segments 0, 1, 3 on TLI 1 — segment 2 is MISSING.
	for _, n := range []uint64{0, 1, 3} {
		putSegmentManifest(t, sp, "db1", 1, walsink.SegmentFileName(1, n, seg))
	}

	// Range covering the hole [seg0 .. seg3] → reports segment 2 missing.
	hole, found, err := inventory.FirstWALHoleInRange(ctx, sp, "db1", 1, 0, pglogrepl.LSN(3*uint64(seg)))
	if err != nil {
		t.Fatal(err)
	}
	if !found || uint64(hole) != 2*uint64(seg) {
		t.Errorf("hole=%#x found=%v, want a hole at %#x (segment 2)", uint64(hole), found, 2*uint64(seg))
	}

	// Range covering only the contiguous prefix [seg0 .. seg1] → no hole.
	if _, found, _ := inventory.FirstWALHoleInRange(ctx, sp, "db1", 1, 0, pglogrepl.LSN(uint64(seg))); found {
		t.Error("contiguous range [seg0,seg1] must report no hole")
	}

	// A timeline with no archived WAL → no hole (geometry unknown; the
	// caller's reachability checks handle a wholly-absent archive).
	if _, found, _ := inventory.FirstWALHoleInRange(ctx, sp, "db1", 9, 0, pglogrepl.LSN(3*uint64(seg))); found {
		t.Error("empty timeline must report no hole")
	}

	// Inverted range → no hole.
	if _, found, _ := inventory.FirstWALHoleInRange(ctx, sp, "db1", 1, pglogrepl.LSN(3*uint64(seg)), 0); found {
		t.Error("inverted range must report no hole")
	}
}
