package backup_test

// Chain protection is what stops SoftDelete from tombstoning a backup
// that live incrementals still descend from. It answers exactly one
// question — "would deleting X orphan anything?" — from a snapshot of
// every live manifest's (backup_id, parent_backup_id) edge.
//
// The snapshot used to SKIP any manifest whose body would not parse,
// with the reasoning "a malformed manifest can't have a usable parent
// reference". That answers a different question. We are not asking
// whether the corrupt manifest is usable; we are asking whether
// deleting something ELSE would strand it. A manifest we cannot parse
// is one whose parent edge we cannot see, so skipping it drops the edge
// from the graph, descendants() comes back empty, and the tombstone
// goes in over a live child sitting right there on disk. The child is
// then a dangling chain link — chain.broken_tombstoned — discovered at
// restore.
//
// The same function already fails CLOSED when the manifest cannot be
// READ (it returns the error), and walprune's frontier walk refuses to
// prune at all rather than risk deleting WAL a manifest it could not
// decode still needs. Unparseable was the odd one out.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// corruptManifestBody overwrites a committed manifest with bytes that
// will not parse, leaving the key present and non-tombstoned.
func corruptManifestBody(t *testing.T, sp storage.StoragePlugin, deployment, id string) {
	t.Helper()
	key := backup.PrimaryPath(deployment, id)
	body := []byte(`{"backup_id":"` + id + `","parent_backup_id":`) // truncated mid-value
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("corrupt %s: %v", key, err)
	}
}

// The regression: A <- B, B's manifest corrupt. Deleting A must not
// succeed, because B descends from it and would be orphaned.
func TestSoftDelete_RefusesWhileAChildManifestIsUnparseable(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
	})
	corruptManifestBody(t, sp, "db1", "B")

	err := store.SoftDelete(context.Background(), "db1", "A", "manual", "test")
	if err == nil {
		t.Fatal("SoftDelete tombstoned A while its child B's manifest was unparseable — B's " +
			"parent edge was invisible to chain protection, so B is now a dangling link and " +
			"its chain cannot be restored")
	}
	if !strings.Contains(err.Error(), "chain protection cannot decode manifest") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
	// The operator needs to know WHICH manifest and what to do.
	for _, want := range []string{"db1", "B", "repair manifest"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q — an operator cannot act on it:\n%s", want, err)
		}
	}
}

// The same must hold for the batch path, which shares the snapshot.
func TestSoftDeleteBatch_RefusesWhileAChildManifestIsUnparseable(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
	})
	corruptManifestBody(t, sp, "db1", "B")

	if _, err := store.SoftDeleteBatch(context.Background(), "db1", []string{"A"},
		"manual", "test"); err == nil {
		t.Fatal("SoftDeleteBatch tombstoned A while its child B was unparseable")
	}
}

// A corrupt manifest that is NOT in the chain must still block, because
// the snapshot cannot tell whose child it is — that is the whole reason
// the edge matters. Stated explicitly so the conservative scope is a
// decision rather than an accident.
func TestSoftDelete_RefusesForAnyUnparseableManifestInTheDeployment(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "Z", btype: backup.BackupTypeFull},
	})
	corruptManifestBody(t, sp, "db1", "Z")

	if err := store.SoftDelete(context.Background(), "db1", "A", "manual", "test"); err == nil {
		t.Fatal("SoftDelete proceeded with an unparseable manifest in the deployment — its " +
			"parent edge is unknown, so it cannot be ruled out as a descendant of A")
	}
}

// And the healthy case must still work, or retention stops entirely.
func TestSoftDelete_StillWorksWithAllManifestsParseable(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
	})
	// B is a leaf: deleting it orphans nothing.
	if err := store.SoftDelete(context.Background(), "db1", "B", "manual", "test"); err != nil {
		t.Fatalf("deleting a leaf must succeed: %v", err)
	}
	// Now A has no live descendants.
	if err := store.SoftDelete(context.Background(), "db1", "A", "manual", "test"); err != nil {
		t.Fatalf("deleting a now-childless anchor must succeed: %v", err)
	}
}

// The pre-existing contract must be unchanged: a live child still
// produces the typed refusal, not the new decode error.
func TestSoftDelete_LiveChildStillGivesTheTypedRefusal(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
	})
	err := store.SoftDelete(context.Background(), "db1", "A", "manual", "test")
	var chErr *backup.ChainHasLiveDescendantsError
	if !errors.As(err, &chErr) {
		t.Fatalf("expected *ChainHasLiveDescendantsError, got %T: %v", err, err)
	}
	if len(chErr.Descendants) != 1 || chErr.Descendants[0] != "B" {
		t.Errorf("Descendants = %v, want [B]", chErr.Descendants)
	}
}
