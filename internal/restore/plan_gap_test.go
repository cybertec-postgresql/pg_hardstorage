package restore_test

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

func planPutSeg(t *testing.T, sp storage.StoragePlugin, deployment string, tli uint32, segNum uint64) {
	t.Helper()
	name := walsink.SegmentFileName(tli, segNum, walsink.SegmentSize)
	start := pglogrepl.LSN(segNum * uint64(walsink.SegmentSize))
	m := &walsink.SegmentManifest{
		Schema:           walsink.Schema,
		Deployment:       deployment,
		SystemIdentifier: "7000000000000000001",
		Timeline:         tli,
		SegmentNumber:    segNum,
		SegmentName:      name,
		StartLSN:         start.String(),
		EndLSN:           (start + pglogrepl.LSN(walsink.SegmentSize)).String(),
		SegmentSize:      walsink.SegmentSize,
	}
	raw, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	key := walsink.SegmentPath(deployment, tli, name)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(raw),
		storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
		t.Fatal(err)
	}
}

// Regression (#5): --preview must surface a WAL archive hole between the
// backup's stop point and the PITR target — the same finding the real
// restore path warns about — instead of reporting "✓ ready". The fixture
// backup stops in segment 3 (StopLSN 0/30001A0); archiving segments 3, 4,
// 6 (segment 5 MISSING) with a target in segment 6 must set
// WALArchiveHoleLSN to segment 5's start.
func TestPreview_SurfacesWALArchiveHole(t *testing.T) {
	fx := newFixture(t)
	root := strings.TrimPrefix(fx.repoURL, "file://")
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	for _, seg := range []uint64{3, 4, 6} { // hole at segment 5
		planPutSeg(t, sp, "db1", 1, seg)
	}

	p, err := restore.Preview(context.Background(), restore.PlanOptions{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.20260428T130000Z.0001",
		TargetDir:  t.TempDir() + "/restored",
		Verifier:   fx.verifier,
		Recovery:   &restore.Recovery{Enable: true, TargetLSN: "0/6000080"}, // in segment 6, past the hole
	})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	const holeStart = "0/5000000" // segment 5 start
	if p.WALArchiveHoleLSN != holeStart {
		t.Errorf("WALArchiveHoleLSN = %q, want %q (preview must not report ✓ ready across a hole)",
			p.WALArchiveHoleLSN, holeStart)
	}
}
