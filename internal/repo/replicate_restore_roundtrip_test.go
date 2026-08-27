package repo_test

// replicate_restore_roundtrip_test.go — the replica's only truth is a
// restore.
//
// Every replicate test until now verified the replica by looking at
// the repository: keys present, counters right, listings matching.
// That is the same class of assertion that let `repo bundle import`
// ship broken for a year — the copies LOOKED right, and nothing ever
// read them back through the one path that matters. A DR replica that
// cannot be restored from is not a replica; it is a bill.
//
// So: commit a backup at the source, replicate, then run the REAL
// restore against the REPLICA and compare the materialised bytes with
// the original content. Chunk-key comparisons cannot stand in for
// this — a bug that corrupts content identically on both sides, or a
// manifest that references chunks the copy never carried, passes
// every listing diff and fails only here.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

func TestReplicate_RestoreFromReplica_ByteIdentical(t *testing.T) {
	w := setupRVWorld(t)
	content := []byte("the only truth a DR replica has is a restore — byte for byte")
	id := w.commitToBoth(t, "db1", 1, content, true /* replicate */)

	target := filepath.Join(t.TempDir(), "restored-from-replica")
	res, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    w.dstURL, // the REPLICA, not the source
		Deployment: "db1",
		BackupID:   id,
		TargetDir:  target,
		Verifier:   w.verifier,
	})
	if err != nil {
		t.Fatalf("restore from replica: %v", err)
	}
	if res.FileCount == 0 {
		t.Fatal("restore from replica materialised zero files")
	}

	got, err := os.ReadFile(filepath.Join(target, "data", id))
	if err != nil {
		t.Fatalf("read materialised file: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("restored bytes differ from the original:\n got: %q\nwant: %q", got, content)
	}
}

// The negative control: restoring the same backup from a replica
// whose chunk was lost must FAIL, not fabricate content. If this ever
// passes, the byte-compare above is testing nothing.
func TestReplicate_RestoreFromReplicaMissingChunk_Fails(t *testing.T) {
	w := setupRVWorld(t)
	content := []byte("negative control payload")
	id := w.commitToBoth(t, "db1", 1, content, true)

	// Sabotage the replica: delete the chunk the manifest references.
	deleted := 0
	for info, err := range w.dstSP.List(context.Background(), "chunks/") {
		if err != nil {
			t.Fatal(err)
		}
		if err := w.dstSP.Delete(context.Background(), info.Key); err != nil {
			t.Fatal(err)
		}
		deleted++
	}
	if deleted == 0 {
		t.Fatal("sabotage found no chunks — the fixture layout changed")
	}

	_, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    w.dstURL,
		Deployment: "db1",
		BackupID:   id,
		TargetDir:  filepath.Join(t.TempDir(), "must-not-exist"),
		Verifier:   w.verifier,
	})
	if err == nil {
		t.Fatal("restore succeeded from a replica missing its chunks")
	}
}
