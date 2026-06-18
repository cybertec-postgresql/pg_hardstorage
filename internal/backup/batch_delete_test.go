package backup_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// countingSP wraps a StoragePlugin and counts Get calls — the
// per-manifest reads a chain scan performs. SoftDeleteBatch must scan a
// BOUNDED multiple of N (the deployment's manifest count) regardless of
// K (how many are deleted); the old per-backup SoftDelete loop was
// O(K·N) because each call re-walked every manifest (twice). See
// CPU-pathology audit #2.
type countingSP struct {
	storage.StoragePlugin
	gets atomic.Int64
}

func (c *countingSP) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	c.gets.Add(1)
	return c.StoragePlugin.Get(ctx, key)
}

// commitFulls commits n independent (parent-less) full backups and
// returns their IDs.
func commitFulls(t *testing.T, store *backup.ManifestStore, signer *backup.Signer, deployment string, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		m := sampleManifest()
		m.BackupID = fmt.Sprintf("%s.full.%04d", deployment, i)
		m.Deployment = deployment
		m.Type = backup.BackupTypeFull
		m.ParentBackupID = ""
		if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
			t.Fatalf("commit %s: %v", m.BackupID, err)
		}
		ids = append(ids, m.BackupID)
	}
	return ids
}

// TestSoftDeleteBatch_ScansLinearNotQuadratic pins the O(N) property:
// deleting K of N backups reads a bounded multiple of N, NOT K·N. This
// fails (read count explodes past the linear bound) if SoftDeleteBatch
// is reimplemented as a per-id SoftDelete loop.
func TestSoftDeleteBatch_ScansLinearNotQuadratic(t *testing.T) {
	_, sp, signer, _ := newStore(t)
	csp := &countingSP{StoragePlugin: sp}
	store := backup.NewManifestStore(csp)

	const n, k = 60, 30
	ids := commitFulls(t, store, signer, "db1", n)

	csp.gets.Store(0) // count only the delete path, not setup
	deleted, err := store.SoftDeleteBatch(context.Background(), "db1", ids[:k], "manual", "policy=test")
	if err != nil {
		t.Fatalf("SoftDeleteBatch: %v", err)
	}
	if len(deleted) != k {
		t.Fatalf("deleted %d, want %d", len(deleted), k)
	}

	gets := csp.gets.Load()
	// Batch = two scans (pre + post) plus per-id hold/tombstone probes:
	// a small constant times N, independent of K. A per-backup loop
	// would be ~2·K·N reads (here ≈ 3600); assert well under that and
	// in the linear band.
	linearBound := int64(8 * n)
	quadratic := int64(2 * k * n)
	if gets > linearBound {
		t.Fatalf("SoftDeleteBatch read %d manifests deleting %d of %d; "+
			"want <= %d (linear in N). A per-backup loop would read ~%d (O(K·N)).",
			gets, k, n, linearBound, quadratic)
	}
	t.Logf("SoftDeleteBatch read %d manifests deleting %d of %d (linear bound %d, O(K·N) would be ~%d)",
		gets, k, n, linearBound, quadratic)

	// All K must be tombstoned; the untouched N-K must stay live.
	for _, id := range ids[:k] {
		dead, err := store.IsTombstoned(context.Background(), "db1", id)
		if err != nil {
			t.Fatal(err)
		}
		if !dead {
			t.Errorf("%s should be tombstoned", id)
		}
	}
	for _, id := range ids[k:] {
		dead, err := store.IsTombstoned(context.Background(), "db1", id)
		if err != nil {
			t.Fatal(err)
		}
		if dead {
			t.Errorf("%s should still be live", id)
		}
	}
}

// TestSoftDeleteBatch_RefusesOrphaningDelete: chain A→B. Deleting only
// {A} would orphan the live descendant B (not in the batch) → the whole
// batch is refused atomically and nothing is tombstoned.
func TestSoftDeleteBatch_RefusesOrphaningDelete(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
	})

	_, err := store.SoftDeleteBatch(context.Background(), "db1", []string{"A"}, "manual", "orphan")
	var chErr *backup.ChainHasLiveDescendantsError
	if !errors.As(err, &chErr) {
		t.Fatalf("expected *ChainHasLiveDescendantsError; got %T (%v)", err, err)
	}
	// Atomic refusal: A must NOT be tombstoned.
	dead, err := store.IsTombstoned(context.Background(), "db1", "A")
	if err != nil {
		t.Fatal(err)
	}
	if dead {
		t.Error("A must stay live after a refused batch")
	}
}

// TestSoftDeleteBatch_AllowsParentWithDescendants: deleting a whole
// chain A→B→C in one batch is fine — every descendant is itself in the
// batch, so nothing is orphaned.
func TestSoftDeleteBatch_AllowsParentWithDescendants(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
		{id: "C", parent: "B", btype: backup.BackupTypeIncremental},
	})

	deleted, err := store.SoftDeleteBatch(context.Background(), "db1", []string{"A", "B", "C"}, "manual", "whole-chain")
	if err != nil {
		t.Fatalf("SoftDeleteBatch whole chain: %v", err)
	}
	if len(deleted) != 3 {
		t.Fatalf("deleted %d, want 3", len(deleted))
	}
	for _, id := range []string{"A", "B", "C"} {
		dead, err := store.IsTombstoned(context.Background(), "db1", id)
		if err != nil {
			t.Fatal(err)
		}
		if !dead {
			t.Errorf("%s should be tombstoned", id)
		}
	}
}

// TestSoftDeleteBatch_AllowsLeafSideDelete: chain A→B→C. Deleting the
// leaf side {B,C} is allowed — A keeps no live descendant gap (B's only
// descendant C is in the batch), and A itself stays live.
func TestSoftDeleteBatch_AllowsLeafSideDelete(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
		{id: "C", parent: "B", btype: backup.BackupTypeIncremental},
	})

	deleted, err := store.SoftDeleteBatch(context.Background(), "db1", []string{"B", "C"}, "manual", "leaf-side")
	if err != nil {
		t.Fatalf("SoftDeleteBatch leaf side: %v", err)
	}
	if len(deleted) != 2 {
		t.Fatalf("deleted %d, want 2", len(deleted))
	}
	if dead, _ := store.IsTombstoned(context.Background(), "db1", "A"); dead {
		t.Error("A (the live root) must not be tombstoned")
	}
}

// TestSoftDeleteBatch_RefusesHeldMember: a legal hold on any member
// refuses the whole batch and tombstones nothing.
func TestSoftDeleteBatch_RefusesHeldMember(t *testing.T) {
	store, _, signer, _ := newStore(t)
	ids := commitFulls(t, store, signer, "db1", 3)
	if err := store.PutHold(context.Background(), "db1", ids[1], "ops", "litigation"); err != nil {
		t.Fatalf("PutHold: %v", err)
	}

	_, err := store.SoftDeleteBatch(context.Background(), "db1", ids, "manual", "policy=test")
	var heldErr *backup.ManifestHeldError
	if !errors.As(err, &heldErr) {
		t.Fatalf("expected *ManifestHeldError; got %T (%v)", err, err)
	}
	// Atomic refusal: not even the unheld members may be tombstoned.
	for _, id := range ids {
		if dead, _ := store.IsTombstoned(context.Background(), "db1", id); dead {
			t.Errorf("%s tombstoned despite a held member in the batch", id)
		}
	}
}

// TestSoftDeleteBatch_IdempotentOnTombstoned: members already
// tombstoned are skipped (not errors) and not re-reported.
func TestSoftDeleteBatch_IdempotentOnTombstoned(t *testing.T) {
	store, _, signer, _ := newStore(t)
	ids := commitFulls(t, store, signer, "db1", 3)
	if err := store.SoftDelete(context.Background(), "db1", ids[0], "manual", "first"); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}

	deleted, err := store.SoftDeleteBatch(context.Background(), "db1", ids, "manual", "policy=test")
	if err != nil {
		t.Fatalf("SoftDeleteBatch: %v", err)
	}
	// ids[0] was already tombstoned → only the other two are freshly installed.
	if len(deleted) != 2 {
		t.Fatalf("freshly deleted %d, want 2 (one was already tombstoned)", len(deleted))
	}
	for _, id := range ids {
		if dead, _ := store.IsTombstoned(context.Background(), "db1", id); !dead {
			t.Errorf("%s should be tombstoned", id)
		}
	}
}
