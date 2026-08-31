package partial_test

// A table's relfilenode is resolved against the LIVE catalog
// (LookupRelfilenodes runs pg_relation_filepath on a real connection),
// but its files come from a BACKUP. Those two disagree whenever the
// relation was rewritten after the backup was taken — VACUUM FULL,
// CLUSTER, TRUNCATE, an ALTER TABLE that rewrites, REINDEX for an
// index. All routine operations.
//
// When they disagree, materialiseRelfilenodeFamily looks up a path the
// manifest does not contain, finds nothing, and returns (nil, 0, nil).
// The caller recorded HeapFiles: nil, HeapBytes: 0, added zero to the
// counters, and returned SUCCESS.
//
// So: the operator asks a data-recovery tool to extract a table from a
// backup, the command exits 0, and the target directory is empty. The
// one signal that something is wrong — files_written: 0 — is a number
// they have to notice and interpret, against a result that otherwise
// reads as clean.
//
// res.NotFound already exists but means something different: the table
// is not in the CATALOG (a typo). A table that exists but whose bytes
// are not in THIS backup is a distinct condition with a distinct
// remedy — pick an older backup, or pass the historical relfilenode
// via --relfilenode-map — so it needs to be reported distinctly rather
// than folded in or, as before, not reported at all.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/partial"
)

func TestPartialRestore_TableRewrittenSinceBackupIsReported(t *testing.T) {
	w := setupPartialWorld(t)
	// The backup holds the table at its OLD relfilenode.
	w.commitWithFiles(t, "db1", "db1.full.rewritten", []fileSpec{
		{"base/16384/2619", [][]byte{[]byte("users-page-0")}},
		{"base/16384/2619_vm", [][]byte{[]byte("users-vm")}},
	})

	target := filepath.Join(t.TempDir(), "extract")
	res, err := partial.Restore(context.Background(), partial.RestoreOptions{
		RepoURL:    w.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.rewritten",
		Verifier:   w.verifier,
		Tables:     []string{"public.users"},
		// The LIVE catalog reports a NEW relfilenode: the table was
		// VACUUM FULL'd after this backup was taken.
		RelfilenodeMap: map[string]partial.Relfilenode{
			"public.users": {
				Schema: "public", Table: "users", Qualified: "public.users",
				Path: "base/16384/99999",
			},
		},
		TargetDir: target,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Nothing was extracted — that part is unavoidable, the bytes are
	// not in this backup. What matters is that it is SAID.
	if res.FilesWritten != 0 {
		t.Fatalf("fixture wrote %d files; it should have matched nothing", res.FilesWritten)
	}
	entries, _ := os.ReadDir(target)
	if len(entries) != 0 {
		t.Fatalf("target dir is not empty: %v", entries)
	}

	if len(res.NotInBackup) == 0 {
		t.Fatal("a table whose relfilenode is absent from the backup was reported as a " +
			"successful extraction with no indication at all — the operator asked a recovery " +
			"tool for a table's data, got an empty directory, and an exit code that says it " +
			"worked. This is what happens after any VACUUM FULL, CLUSTER or TRUNCATE " +
			"between the backup and the restore.")
	}
	if res.NotInBackup[0] != "public.users" {
		t.Errorf("NotInBackup = %v, want [public.users]", res.NotInBackup)
	}
	// And it must NOT be conflated with a catalog miss, whose remedy
	// is different.
	if len(res.NotFound) != 0 {
		t.Errorf("NotFound = %v — the table WAS resolved; it is the backup that lacks its "+
			"files, and the two conditions have different remedies", res.NotFound)
	}
}

// The happy path must stay clean, or every ordinary extraction starts
// reporting a problem.
func TestPartialRestore_PresentTableIsNotReportedMissing(t *testing.T) {
	w := setupPartialWorld(t)
	w.commitWithFiles(t, "db1", "db1.full.present", []fileSpec{
		{"base/16384/2619", [][]byte{[]byte("users-page-0")}},
	})

	target := filepath.Join(t.TempDir(), "extract")
	res, err := partial.Restore(context.Background(), partial.RestoreOptions{
		RepoURL:    w.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.present",
		Verifier:   w.verifier,
		Tables:     []string{"public.users"},
		RelfilenodeMap: map[string]partial.Relfilenode{
			"public.users": {
				Schema: "public", Table: "users", Qualified: "public.users",
				Path: "base/16384/2619",
			},
		},
		TargetDir: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.NotInBackup) != 0 {
		t.Errorf("NotInBackup = %v for a table that WAS extracted", res.NotInBackup)
	}
	if res.FilesWritten != 1 {
		t.Errorf("FilesWritten = %d, want 1", res.FilesWritten)
	}
}

// A TOAST relfilenode absent from the backup while the heap is present
// is the more insidious shape: the table restores, looks populated, and
// every out-of-line value is gone.
func TestPartialRestore_MissingToastIsReported(t *testing.T) {
	w := setupPartialWorld(t)
	w.commitWithFiles(t, "db1", "db1.full.toast", []fileSpec{
		{"base/16384/2619", [][]byte{[]byte("users-page-0")}},
	})

	target := filepath.Join(t.TempDir(), "extract")
	res, err := partial.Restore(context.Background(), partial.RestoreOptions{
		RepoURL:    w.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.toast",
		Verifier:   w.verifier,
		Tables:     []string{"public.users"},
		RelfilenodeMap: map[string]partial.Relfilenode{
			"public.users": {
				Schema: "public", Table: "users", Qualified: "public.users",
				Path:      "base/16384/2619",
				ToastPath: "base/16384/2620", // not in the backup
			},
		},
		TargetDir: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.NotInBackup) == 0 {
		t.Fatal("the heap was extracted but its TOAST relfilenode is absent from the backup " +
			"and nothing said so — the table would restore looking populated with every " +
			"out-of-line value missing")
	}
}

// The reported file list must not depend on map iteration order: it is
// output, and two runs over one backup have to agree.
func TestPartialRestore_HeapFileListIsOrdered(t *testing.T) {
	w := setupPartialWorld(t)
	w.commitWithFiles(t, "db1", "db1.full.order", []fileSpec{
		{"base/16384/2619", [][]byte{[]byte("p0")}},
		{"base/16384/2619.1", [][]byte{[]byte("p1")}},
		{"base/16384/2619.2", [][]byte{[]byte("p2")}},
		{"base/16384/2619_fsm", [][]byte{[]byte("fsm")}},
		{"base/16384/2619_vm", [][]byte{[]byte("vm")}},
	})

	var first []string
	for i := 0; i < 20; i++ {
		target := filepath.Join(t.TempDir(), "extract")
		res, err := partial.Restore(context.Background(), partial.RestoreOptions{
			RepoURL:    w.repoURL,
			Deployment: "db1",
			BackupID:   "db1.full.order",
			Verifier:   w.verifier,
			Tables:     []string{"public.users"},
			RelfilenodeMap: map[string]partial.Relfilenode{
				"public.users": {
					Schema: "public", Table: "users", Qualified: "public.users",
					Path: "base/16384/2619",
				},
			},
			TargetDir: target,
		})
		if err != nil {
			t.Fatal(err)
		}
		got := res.Mappings[0].HeapFiles
		if i == 0 {
			first = append([]string(nil), got...)
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("run %d returned %d files, run 0 returned %d", i, len(got), len(first))
		}
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("run %d file list differs from run 0 at %d (%q vs %q) — the family "+
					"walk iterates a map, so `partial restore` reports a different order "+
					"each time and two runs over one backup cannot be diffed",
					i, j, got[j], first[j])
			}
		}
	}
}
