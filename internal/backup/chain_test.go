package backup_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// mkChild builds an incremental manifest pointing at parentID.
// Distinct shape from sampleManifest so it's clear which scaffolding
// the chain-protection tests rely on.
func mkChild(t *testing.T, id, parentID string) *backup.Manifest {
	t.Helper()
	m := *sampleManifest()
	m.BackupID = id
	m.Type = backup.BackupTypeIncremental
	m.ParentBackupID = parentID
	m.StoppedAt = time.Now().UTC()
	return &m
}

// TestSoftDelete_ChainProtection_RefusesAncestorWithLiveChild:
// SoftDelete on a full whose incremental child is still live must
// return a typed *ChainHasLiveDescendantsError listing the
// descendants. This is the production safeguard against the
// "manual delete broke an incremental chain" failure mode.
func TestSoftDelete_ChainProtection_RefusesAncestorWithLiveChild(t *testing.T) {
	store, _, signer, _ := newStore(t)

	full := sampleManifest()
	full.BackupID = "db1.full.A"
	if err := store.Commit(context.Background(), full, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	child := mkChild(t, "db1.inc.B", full.BackupID)
	if err := store.Commit(context.Background(), child, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	err := store.SoftDelete(context.Background(), "db1", full.BackupID, "manual", "test")
	if err == nil {
		t.Fatal("expected SoftDelete to refuse: full has a live incremental descendant")
	}
	if !errors.Is(err, backup.ErrChainHasLiveDescendants) {
		t.Errorf("expected errors.Is(ErrChainHasLiveDescendants); got %v", err)
	}
	var chErr *backup.ChainHasLiveDescendantsError
	if !errors.As(err, &chErr) {
		t.Fatalf("expected *ChainHasLiveDescendantsError; got %T", err)
	}
	if chErr.Deployment != "db1" || chErr.BackupID != full.BackupID {
		t.Errorf("error subject = %s/%s, want db1/%s", chErr.Deployment, chErr.BackupID, full.BackupID)
	}
	if len(chErr.Descendants) != 1 || chErr.Descendants[0] != child.BackupID {
		t.Errorf("Descendants = %v, want [%s]", chErr.Descendants, child.BackupID)
	}
}

// TestSoftDelete_ChainProtection_AllowsLeafFirst: deleting the leaf
// first then the anchor is the supported workflow. This proves the
// chain-protection allows the operator to drain a chain in the
// correct order, not the wrong one.
func TestSoftDelete_ChainProtection_AllowsLeafFirst(t *testing.T) {
	store, _, signer, _ := newStore(t)

	full := sampleManifest()
	full.BackupID = "db1.full.A"
	if err := store.Commit(context.Background(), full, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	child := mkChild(t, "db1.inc.B", full.BackupID)
	if err := store.Commit(context.Background(), child, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	// Leaf first — should succeed.
	if err := store.SoftDelete(context.Background(), "db1", child.BackupID, "manual", "test"); err != nil {
		t.Fatalf("leaf-first SoftDelete: %v", err)
	}
	// Now the anchor — should also succeed (no live descendants).
	if err := store.SoftDelete(context.Background(), "db1", full.BackupID, "manual", "test"); err != nil {
		t.Fatalf("anchor-after-leaf SoftDelete: %v", err)
	}
}

// TestSoftDelete_ChainProtection_TransitiveDescendants: SoftDelete
// on a full with a 2-link chain (full → inc1 → inc2) sees BOTH
// descendants and refuses. Validates that the BFS walks the chain
// transitively, not just the direct child.
func TestSoftDelete_ChainProtection_TransitiveDescendants(t *testing.T) {
	store, _, signer, _ := newStore(t)

	full := sampleManifest()
	full.BackupID = "db1.full.A"
	if err := store.Commit(context.Background(), full, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	inc1 := mkChild(t, "db1.inc.B", full.BackupID)
	if err := store.Commit(context.Background(), inc1, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	inc2 := mkChild(t, "db1.inc.C", inc1.BackupID)
	if err := store.Commit(context.Background(), inc2, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	err := store.SoftDelete(context.Background(), "db1", full.BackupID, "manual", "test")
	var chErr *backup.ChainHasLiveDescendantsError
	if !errors.As(err, &chErr) {
		t.Fatalf("expected *ChainHasLiveDescendantsError; got %v", err)
	}
	if len(chErr.Descendants) != 2 {
		t.Errorf("want 2 transitive descendants; got %v", chErr.Descendants)
	}
	// Both leaf and intermediate must appear.
	got := strings.Join(chErr.Descendants, ",")
	for _, want := range []string{inc1.BackupID, inc2.BackupID} {
		if !strings.Contains(got, want) {
			t.Errorf("want %s in descendants; got %s", want, got)
		}
	}
}

// TestSoftDelete_ChainProtection_TombstonedDescendantsDontCount: a
// chain where the leaf is already tombstoned shouldn't block the
// anchor's deletion. Tombstoned manifests are slated for GC; they
// don't anchor anything.
func TestSoftDelete_ChainProtection_TombstonedDescendantsDontCount(t *testing.T) {
	store, _, signer, _ := newStore(t)

	full := sampleManifest()
	full.BackupID = "db1.full.A"
	if err := store.Commit(context.Background(), full, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	child := mkChild(t, "db1.inc.B", full.BackupID)
	if err := store.Commit(context.Background(), child, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := store.SoftDelete(context.Background(), "db1", child.BackupID, "manual", "test"); err != nil {
		t.Fatalf("tombstone leaf: %v", err)
	}
	// Anchor delete should now succeed: no LIVE descendants remain.
	if err := store.SoftDelete(context.Background(), "db1", full.BackupID, "manual", "test"); err != nil {
		t.Errorf("expected anchor delete to succeed after leaf tombstone; got %v", err)
	}
}

// TestSoftDelete_ChainProtection_FullWithoutChildPasses: the
// chain-protection scan must not introduce a regression for the
// regular case (no incrementals at all). A simple full backup
// SoftDelete still works.
func TestSoftDelete_ChainProtection_FullWithoutChildPasses(t *testing.T) {
	store, _, signer, _ := newStore(t)

	full := sampleManifest()
	full.BackupID = "db1.full.A"
	if err := store.Commit(context.Background(), full, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := store.SoftDelete(context.Background(), "db1", full.BackupID, "manual", "test"); err != nil {
		t.Errorf("simple full SoftDelete should pass; got %v", err)
	}
}

// TestSoftDelete_ChainProtection_SiblingChainsIndependent: two
// independent chains in the same deployment shouldn't cross-block.
// Deleting the anchor of chain X must not be blocked by descendants
// of chain Y.
func TestSoftDelete_ChainProtection_SiblingChainsIndependent(t *testing.T) {
	store, _, signer, _ := newStore(t)

	fullX := sampleManifest()
	fullX.BackupID = "db1.full.X"
	if err := store.Commit(context.Background(), fullX, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	fullY := sampleManifest()
	fullY.BackupID = "db1.full.Y"
	if err := store.Commit(context.Background(), fullY, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	// Y has a child; X doesn't.
	childY := mkChild(t, "db1.inc.Y1", fullY.BackupID)
	if err := store.Commit(context.Background(), childY, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	// Deleting fullX must succeed (no live descendants of X).
	if err := store.SoftDelete(context.Background(), "db1", fullX.BackupID, "manual", "test"); err != nil {
		t.Errorf("sibling delete should not be blocked by other chain; got %v", err)
	}
	// Y still blocked by its child.
	err := store.SoftDelete(context.Background(), "db1", fullY.BackupID, "manual", "test")
	if !errors.Is(err, backup.ErrChainHasLiveDescendants) {
		t.Errorf("Y delete should be blocked by its child; got %v", err)
	}
}
