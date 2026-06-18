package runner

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

// newSP builds a temp file:// SP for the gap-embedding tests.
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

// putGap writes one gapstate.Record. Real Coordinators write
// these in production via persistGap; tests synthesise.
func putGap(t *testing.T, sp storage.StoragePlugin, deployment string, tli uint32, startLSN, endLSN string, bytes uint64, at time.Time) {
	t.Helper()
	s := gapstate.NewWithClock(sp, func() time.Time { return at })
	if _, err := s.Put(context.Background(), gapstate.Record{
		Deployment:  deployment,
		SlotName:    "slot",
		SlotRole:    "leader",
		Timeline:    tli,
		GapStartLSN: startLSN,
		GapEndLSN:   endLSN,
		GapBytes:    bytes,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestReadGapsForManifest_NoGaps: clean repo → nil result, nil
// error. Empty omitempty on the manifest keeps the JSON shape
// compact for the common case.
func TestReadGapsForManifest_NoGaps(t *testing.T) {
	sp := newSP(t)
	got, err := readGapsForManifest(context.Background(), sp, "db1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil result; got %+v", got)
	}
}

// TestReadGapsForManifest_RoundTrip: a planted gap shows up
// with all fields preserved. Validates that the live →
// manifest-form mapping is identity-preserving.
func TestReadGapsForManifest_RoundTrip(t *testing.T) {
	sp := newSP(t)
	at := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	putGap(t, sp, "db1", 7, "0/3000028", "0/30001A0", 420, at)

	got, err := readGapsForManifest(context.Background(), sp, "db1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	g := got[0]
	if g.SlotName != "slot" {
		t.Errorf("SlotName = %q", g.SlotName)
	}
	if g.SlotRole != "leader" {
		t.Errorf("SlotRole = %q", g.SlotRole)
	}
	if g.Timeline != 7 {
		t.Errorf("Timeline = %d", g.Timeline)
	}
	if g.GapStartLSN != "0/3000028" {
		t.Errorf("GapStartLSN = %q", g.GapStartLSN)
	}
	if g.GapEndLSN != "0/30001A0" {
		t.Errorf("GapEndLSN = %q", g.GapEndLSN)
	}
	if g.GapBytes != 420 {
		t.Errorf("GapBytes = %d", g.GapBytes)
	}
	if !g.DetectedAt.Equal(at) {
		t.Errorf("DetectedAt = %v, want %v", g.DetectedAt, at)
	}
}

// TestReadGapsForManifest_MultipleGaps: every gap on the
// deployment is included, regardless of TLI. The manifest
// embeds the full forensic trail at backup commit time.
func TestReadGapsForManifest_MultipleGaps(t *testing.T) {
	sp := newSP(t)
	t1 := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 4, 30, 13, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 4, 30, 14, 0, 0, 0, time.UTC)
	putGap(t, sp, "db1", 1, "0/100", "0/200", 256, t1)
	putGap(t, sp, "db1", 5, "0/300", "0/400", 256, t2)
	putGap(t, sp, "db1", 9, "0/500", "0/600", 256, t3)

	got, err := readGapsForManifest(context.Background(), sp, "db1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("len = %d, want 3 (all TLIs included)", len(got))
	}
}

// TestReadGapsForManifest_DeploymentScoped: gaps on a different
// deployment must NOT leak into this manifest.
func TestReadGapsForManifest_DeploymentScoped(t *testing.T) {
	sp := newSP(t)
	at := time.Now().UTC()
	putGap(t, sp, "db1", 1, "0/100", "0/200", 256, at)
	putGap(t, sp, "db2", 1, "0/100", "0/200", 999, at)

	got, err := readGapsForManifest(context.Background(), sp, "db1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].GapBytes != 256 {
		t.Errorf("GapBytes = %d, want 256 (db2's gap should not leak)", got[0].GapBytes)
	}
}
