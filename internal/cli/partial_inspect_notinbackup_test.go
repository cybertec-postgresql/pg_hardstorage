package cli

// partial_inspect_notinbackup_test.go — the preflight must not answer
// "yes, 0 bytes".
//
// `partial inspect --tables` exists to answer, in the package doc's own
// words, "would my partial restore work, and how big is it?" — before
// the operator commits to the restore.
//
// It resolves each table's relfilenode against the LIVE catalog and
// matches that path against a BACKUP's manifest. Those two disagree
// whenever the relation was rewritten after the backup was taken:
// VACUUM FULL, CLUSTER, TRUNCATE, a rewriting ALTER TABLE. All routine.
//
// When they disagreed, matchManifestPaths found no FileEntry under the
// path and left the mapping at heap_bytes 0, heap_chunks 0,
// heap_segments 0, not_found false — reported as a table that IS in the
// backup and happens to be empty. The restore path already learned to
// call this NotInBackup; the view an operator consults FIRST still said
// the restore would work.
//
// The discriminator is segments, not bytes: an empty table still has a
// heap file in the manifest (a FileEntry of size 0), so zero segments
// means the path is genuinely absent rather than the relation being
// empty. That distinction is what this test pins.

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/partial"
)

func inspectManifest(files ...backup.FileEntry) *backup.Manifest {
	return &backup.Manifest{Files: files}
}

func TestMatchManifestPaths_RewrittenTableIsFlaggedNotInBackup(t *testing.T) {
	// The backup holds the table at its OLD relfilenode (2619); the
	// live catalog now reports the NEW one (99999) after a rewrite.
	m := inspectManifest(
		backup.FileEntry{Path: "base/16384/2619", Size: 8192},
		backup.FileEntry{Path: "base/16384/2619.1", Size: 4096},
	)
	got := matchManifestPaths(m, []partial.Relfilenode{{
		Qualified: "public.users", Path: "base/16384/99999", Relfilenode: 99999,
	}})
	if len(got) != 1 {
		t.Fatalf("got %d mappings, want 1", len(got))
	}
	e := got[0]
	if !e.NotInBackup {
		t.Fatalf("public.users reported as present with heap_bytes=%d heap_segments=%d.\n\n"+
			"The preflight whose job is answering \"would my partial restore work\" answers "+
			"\"yes, 0 bytes\" for a table the restore cannot produce — indistinguishable from "+
			"an empty table.", e.HeapBytes, e.HeapSegments)
	}
	if e.NotFound {
		t.Error("NotInBackup must not be conflated with NotFound: one means the table is " +
			"absent from the catalog (a typo), the other that its bytes are not in THIS " +
			"backup, and the remedies differ")
	}
}

// A table that IS in the backup must not be flagged, including a
// genuinely EMPTY one — a zero-byte heap file is still a heap file, and
// flagging it would send operators chasing a rewrite that never
// happened.
func TestMatchManifestPaths_PresentTablesAreNotFlagged(t *testing.T) {
	cases := map[string]backup.FileEntry{
		"populated":   {Path: "base/16384/2619", Size: 8192},
		"empty table": {Path: "base/16384/2619", Size: 0},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			got := matchManifestPaths(inspectManifest(f), []partial.Relfilenode{{
				Qualified: "public.users", Path: "base/16384/2619",
			}})
			if got[0].NotInBackup {
				t.Errorf("%s flagged as not-in-backup; the heap file IS in the manifest "+
					"(segments=%d, bytes=%d)", name, got[0].HeapSegments, got[0].HeapBytes)
			}
		})
	}
}

// A table missing from the catalog keeps its own signal and does not
// pick up the new one — nothing was looked up, so nothing can be said
// about the backup.
func TestMatchManifestPaths_NotFoundTableIsNotAlsoNotInBackup(t *testing.T) {
	got := matchManifestPaths(inspectManifest(), []partial.Relfilenode{{
		Qualified: "public.typo", NotFound: true,
	}})
	if !got[0].NotFound {
		t.Fatal("NotFound was dropped")
	}
	if got[0].NotInBackup {
		t.Error("a table absent from the catalog was also flagged not-in-backup; the two " +
			"conditions have different remedies and must stay distinguishable")
	}
}

// Multi-segment relations (>1 GiB) match by prefix; the flag must
// account for every segment, not just the base path.
func TestMatchManifestPaths_SegmentedRelationCounts(t *testing.T) {
	m := inspectManifest(
		backup.FileEntry{Path: "base/16384/2619", Size: 1 << 30},
		backup.FileEntry{Path: "base/16384/2619.1", Size: 1 << 30},
		backup.FileEntry{Path: "base/16384/2619.2", Size: 512},
	)
	got := matchManifestPaths(m, []partial.Relfilenode{{
		Qualified: "public.big", Path: "base/16384/2619",
	}})
	if got[0].NotInBackup {
		t.Fatal("a segmented relation was flagged not-in-backup")
	}
	if got[0].HeapSegments != 3 {
		t.Errorf("HeapSegments = %d, want 3", got[0].HeapSegments)
	}
}
