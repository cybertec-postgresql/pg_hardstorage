package bundle_test

// --include-wal must not silently produce a bundle with no WAL.
//
// The export draws its segment list from each manifest's WALRequired,
// and nothing populates that field: the backup runner sets
// `WALRequired: nil` with the note "empty in v0.1: WAL streaming lands
// in Slice 8". WAL streaming has since landed; the field did not follow.
// BundleManifest.Timelines — declared as "one timeline-history file the
// bundle carries" — is never populated by anything either.
//
// So `repo bundle export --include-wal` reported success, carried the
// base backups, and contained not one WAL segment. A base backup cannot
// reach a consistent state without the WAL between its start and stop
// LSN, so every such bundle was unrestorable — discovered at restore
// time, from the air-gapped copy, which is the worst possible moment.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/bundle"
)

// Backups record no wal_required (issue #77), so the export derives the
// segments from the start..stop LSN range and carries them.
func TestExport_IncludeWALDerivesSegmentsWhenManifestRecordsNone(t *testing.T) {
	sp := newRepo(t)
	m := sampleManifest(t, sp, "db1.full.20260501T120000Z")
	m.WALRequired = nil // production shape; LSNs 0/3000028..0/30001A0 -> segment 3
	commitManifest(t, sp, m)
	walKey := "wal/db1/00000001/000000010000000000000003.json"
	body := `{"schema":"pg_hardstorage.wal_segment.v1"}`
	if _, err := sp.Put(context.Background(), walKey, strings.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	bm, err := bundle.Export(context.Background(), sp, &buf, bundle.ExportOptions{
		Deployment: "db1",
		IncludeWAL: true,
	})
	if err != nil {
		t.Fatalf("export --include-wal failed: %v", err)
	}
	if len(bm.WAL) != 1 || bm.WAL[0].Filename != "000000010000000000000003" {
		t.Fatalf("WAL entries = %+v, want segment ...0003", bm.WAL)
	}
}

// A segment the backup needs but the repository lacks must fail the
// export: the bundle would be unrestorable.
func TestExport_IncludeWALRefusesWhenSegmentNotArchived(t *testing.T) {
	sp := newRepo(t)
	m := sampleManifest(t, sp, "db1.full.20260501T120000Z")
	m.WALRequired = nil
	commitManifest(t, sp, m)

	var buf bytes.Buffer
	_, err := bundle.Export(context.Background(), sp, &buf, bundle.ExportOptions{
		Deployment: "db1",
		IncludeWAL: true,
	})
	if err == nil {
		t.Fatal("--include-wal reported success with the required WAL missing")
	}
	for _, want := range []string{"000000010000000000000003", "--include-wal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q:\n%v", want, err)
		}
	}
}

// Without the flag, the same repository exports fine — the refusal is
// scoped to the unmet claim, not to bundling in general.
func TestExport_WithoutIncludeWALStillExports(t *testing.T) {
	sp := newRepo(t)
	m := sampleManifest(t, sp, "db1.full.20260501T120000Z")
	m.WALRequired = nil
	commitManifest(t, sp, m)

	var buf bytes.Buffer
	bm, err := bundle.Export(context.Background(), sp, &buf, bundle.ExportOptions{
		Deployment: "db1",
	})
	if err != nil {
		t.Fatalf("export without --include-wal failed: %v", err)
	}
	if len(bm.Backups) == 0 {
		t.Error("bundle carried no backups")
	}
	if buf.Len() == 0 {
		t.Error("bundle tar is empty")
	}
}
