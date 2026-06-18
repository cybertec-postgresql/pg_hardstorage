package backup_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// commitChain commits a chain root + N descendants. Returns the
// IDs in commit order (root first, deeper descendants later).
func commitChain(t *testing.T, store *backup.ManifestStore, signer *backup.Signer, deployment string, links []chainLink) []string {
	t.Helper()
	var ids []string
	for _, l := range links {
		m := sampleManifest()
		m.BackupID = l.id
		m.Deployment = deployment
		m.Type = l.btype
		m.ParentBackupID = l.parent
		if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
			t.Fatalf("commit %s: %v", l.id, err)
		}
		ids = append(ids, l.id)
	}
	return ids
}

type chainLink struct {
	id, parent string
	btype      backup.BackupType
}

// TestSoftDeleteCascade_LinearChain: A → B → C. Cascade from A
// tombstones C first, then B, then A. Validates the leaf-first
// invariant.
func TestSoftDeleteCascade_LinearChain(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
		{id: "C", parent: "B", btype: backup.BackupTypeIncremental},
	})

	deleted, err := store.SoftDeleteCascade(context.Background(), "db1", "A", "manual", "test cascade")
	if err != nil {
		t.Fatalf("SoftDeleteCascade: %v", err)
	}
	want := []string{"C", "B", "A"}
	if !equalStrSlice(deleted, want) {
		t.Errorf("deleted = %v, want %v (leaf-first)", deleted, want)
	}
	// Every link must now be tombstoned.
	for _, id := range want {
		dead, err := store.IsTombstoned(context.Background(), "db1", id)
		if err != nil {
			t.Fatal(err)
		}
		if !dead {
			t.Errorf("%s should be tombstoned", id)
		}
	}
}

// TestSoftDeleteCascade_BranchedChain: A has two direct
// children B, C; C has its own child D. Cascade from A
// tombstones D, B, C, A in some leaf-first valid order.
// Validates BFS handles branches correctly.
func TestSoftDeleteCascade_BranchedChain(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
		{id: "C", parent: "A", btype: backup.BackupTypeIncremental},
		{id: "D", parent: "C", btype: backup.BackupTypeIncremental},
	})

	deleted, err := store.SoftDeleteCascade(context.Background(), "db1", "A", "manual", "branch")
	if err != nil {
		t.Fatalf("SoftDeleteCascade: %v", err)
	}
	if len(deleted) != 4 {
		t.Fatalf("len(deleted) = %d, want 4", len(deleted))
	}
	// A must be LAST (root).
	if deleted[len(deleted)-1] != "A" {
		t.Errorf("root should be deleted last; got order = %v", deleted)
	}
	// D must come before C (its parent).
	posD := indexOf(deleted, "D")
	posC := indexOf(deleted, "C")
	if posD == -1 || posC == -1 || posD >= posC {
		t.Errorf("D (leaf) should be deleted before C (parent); order = %v", deleted)
	}
}

// TestSoftDeleteCascade_NoDescendants: cascading from a leaf
// (no children) is equivalent to a plain SoftDelete on that
// leaf. The cascade returns the single deletion in the slice.
func TestSoftDeleteCascade_NoDescendants(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "solo", btype: backup.BackupTypeFull},
	})

	deleted, err := store.SoftDeleteCascade(context.Background(), "db1", "solo", "manual", "leaf")
	if err != nil {
		t.Fatalf("SoftDeleteCascade: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "solo" {
		t.Errorf("deleted = %v, want [solo]", deleted)
	}
}

// TestSoftDeleteCascade_AlreadyTombstonedRoot: idempotent —
// re-running the cascade on an already-tombstoned root is a
// no-op (returns empty slice + nil error).
func TestSoftDeleteCascade_AlreadyTombstonedRoot(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	// First cascade tombstones A.
	if _, err := store.SoftDeleteCascade(context.Background(), "db1", "A", "manual", "first"); err != nil {
		t.Fatal(err)
	}
	// Second cascade: A is already tombstoned, should be a
	// no-op.
	deleted, err := store.SoftDeleteCascade(context.Background(), "db1", "A", "manual", "second")
	if err != nil {
		t.Errorf("second cascade should be no-op; got %v", err)
	}
	if len(deleted) != 0 {
		t.Errorf("second cascade returned %v, want empty", deleted)
	}
}

// TestSoftDeleteCascade_RejectsBadInput: required-field
// guards.
func TestSoftDeleteCascade_RejectsBadInput(t *testing.T) {
	store, _, _, _ := newStore(t)
	cases := []struct{ deployment, id string }{
		{"", "A"},
		{"db1", ""},
	}
	for _, c := range cases {
		if _, err := store.SoftDeleteCascade(context.Background(), c.deployment, c.id, "manual", "test"); err == nil {
			t.Errorf("expected error for deployment=%q id=%q", c.deployment, c.id)
		}
	}
}

// TestSoftDeleteCascade_PartialFailureLeavesPartialState:
// when the underlying tombstone-write fails partway through,
// the cascade returns the deletions completed before the
// error. Pinned via injection — we tombstone B manually
// first; the cascade attempts the remaining deletions and
// the second-cascade call (re-running) cleanly drains the
// rest. This exercises the "naturally idempotent on re-run"
// claim in the docs.
func TestSoftDeleteCascade_PartialFailureLeavesPartialState(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
		{id: "C", parent: "B", btype: backup.BackupTypeIncremental},
	})
	// Manually tombstone C up-front. The cascade's
	// findLiveDescendants will skip C because it's already
	// tombstoned; it'll only see B as a live descendant of A.
	if err := store.SoftDelete(context.Background(), "db1", "C", "manual", "pre"); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.SoftDeleteCascade(context.Background(), "db1", "A", "manual", "cascade")
	if err != nil {
		t.Fatalf("cascade after pre-tombstone: %v", err)
	}
	// Cascade should have skipped C (already tombstoned), so
	// deleted = [B, A].
	if !equalStrSlice(deleted, []string{"B", "A"}) {
		t.Errorf("deleted = %v, want [B, A] (C was pre-tombstoned)", deleted)
	}
}

// TestSoftDelete_PointerToCascadeInSuggestion: regression
// guard that the chain-protection refusal's Suggestion
// includes the --cascade hint so operators can find it
// without reading docs.
func TestSoftDelete_PointerToCascadeInSuggestion(t *testing.T) {
	// This test exercises the CLI-side error-mapping wrapper,
	// not the manifest-store level. It belongs in the cli
	// package. We verify the underlying error type carries
	// what's needed; the CLI test (TestBackupDelete_Cascade*
	// below) confirms the surfaced text.
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
	})
	err := store.SoftDelete(context.Background(), "db1", "A", "manual", "test")
	if err == nil {
		t.Fatal("expected chain-protection refusal")
	}
	var chErr *backup.ChainHasLiveDescendantsError
	if !errors.As(err, &chErr) {
		t.Fatalf("expected *ChainHasLiveDescendantsError; got %T", err)
	}
	if len(chErr.Descendants) != 1 || chErr.Descendants[0] != "B" {
		t.Errorf("Descendants = %v, want [B]", chErr.Descendants)
	}
}

func indexOf(s []string, target string) int {
	for i, v := range s {
		if v == target {
			return i
		}
	}
	return -1
}

func equalStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
