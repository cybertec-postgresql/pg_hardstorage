package backup_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// Undelete used to check only chunk existence: resurrecting an
// incremental whose parent chain stayed tombstoned handed back a
// live-listing backup that every restore refuses
// (chain.broken_tombstoned) — and once the parent's tombstone aged
// past GC grace, its chunks and WAL were reaped, making the
// resurrected leaf PERMANENTLY unrestorable while presenting as
// healthy. Undelete must fail closed naming the dead ancestor.
func TestUndelete_RefusesWhenAncestorTombstoned(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "F", btype: backup.BackupTypeFull},
		{id: "I1", parent: "F", btype: backup.BackupTypeIncremental},
	})
	seedSampleChunks(t, sp)

	if _, err := store.SoftDeleteCascade(context.Background(), "db1", "F", "manual", "test"); err != nil {
		t.Fatalf("cascade delete: %v", err)
	}

	// The natural "get my newest backup back" move: undelete the leaf.
	_, err := store.Undelete(context.Background(), "db1", "I1")
	if err == nil {
		t.Fatal("Undelete resurrected an incremental whose full parent is still tombstoned — unrestorable-but-live backup")
	}
	var pe *backup.UndeleteParentTombstonedError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want UndeleteParentTombstonedError", err)
	}
	if pe.Ancestor != "F" {
		t.Errorf("Ancestor = %q, want F", pe.Ancestor)
	}
	// The leaf's tombstone must remain (fail closed, no half-state).
	if dead, _ := store.IsTombstoned(context.Background(), "db1", "I1"); !dead {
		t.Error("I1 tombstone removed despite the refusal")
	}

	// Correct order works: parent first, then the leaf.
	if _, err := store.Undelete(context.Background(), "db1", "F"); err != nil {
		t.Fatalf("undelete parent: %v", err)
	}
	if _, err := store.Undelete(context.Background(), "db1", "I1"); err != nil {
		t.Fatalf("undelete leaf after parent: %v", err)
	}
}
