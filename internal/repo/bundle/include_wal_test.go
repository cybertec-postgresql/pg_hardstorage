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

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/bundle"
)

func TestExport_IncludeWALRefusesWhenNoManifestDeclaresAny(t *testing.T) {
	sp := newRepo(t)
	// PRODUCTION shape: the backup runner sets WALRequired to nil. The
	// package's own sampleManifest populates it, which is why this gap
	// went unnoticed — the bundle tests exercise a manifest shape no
	// backup this build writes.
	m := sampleManifest(t, sp, "db1.full.20260501T120000Z")
	m.WALRequired = nil
	commitManifest(t, sp, m)

	var buf bytes.Buffer
	_, err := bundle.Export(context.Background(), sp, &buf, bundle.ExportOptions{
		Deployment: "db1",
		IncludeWAL: true,
	})
	if err == nil {
		t.Fatal("--include-wal produced a bundle with no WAL segments and reported success.\n\n" +
			"The base backups it carries cannot be brought to a consistent state without " +
			"the WAL between their start and stop LSN, so the bundle is unrestorable — and " +
			"the operator finds out from the air-gapped copy.")
	}
	for _, want := range []string{"wal_required", "--include-wal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q, so the operator cannot tell why:\n%v", want, err)
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
