package backup_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// seedSampleChunks stores the chunk bodies that sampleManifest (and
// thus commitChain) references — the byte strings "alpha" and "beta"
// — so a restorability-checked Undelete of those backups sees every
// referenced chunk present. PutChunk is DurabilityInline by default,
// so the chunks are immediately Stat-able.
func seedSampleChunks(t *testing.T, sp storage.StoragePlugin) {
	t.Helper()
	cas := repo.NewCAS(sp)
	for _, body := range [][]byte{[]byte("alpha"), []byte("beta")} {
		if _, err := cas.PutChunk(context.Background(), body); err != nil {
			t.Fatalf("seed sample chunk %q: %v", body, err)
		}
	}
}

// TestUndelete_RestoresTombstoned: SoftDelete then Undelete. The
// manifest reappears in List and Read no longer returns
// ErrTombstoned.
func TestUndelete_RestoresTombstoned(t *testing.T) {
	store, sp, signer, verifier := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	seedSampleChunks(t, sp) // chunks present → restorability pre-flight passes
	if err := store.SoftDelete(context.Background(), "db1", "A", "manual", "test"); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	// Confirm tombstoned.
	dead, err := store.IsTombstoned(context.Background(), "db1", "A")
	if err != nil {
		t.Fatal(err)
	}
	if !dead {
		t.Fatal("manifest should be tombstoned after SoftDelete")
	}

	restored, err := store.Undelete(context.Background(), "db1", "A")
	if err != nil {
		t.Fatalf("Undelete: %v", err)
	}
	if !restored {
		t.Errorf("Undelete returned restored=false on a tombstoned manifest; want true")
	}

	// Now alive again: List sees it; Read returns the manifest.
	dead, err = store.IsTombstoned(context.Background(), "db1", "A")
	if err != nil {
		t.Fatal(err)
	}
	if dead {
		t.Errorf("manifest should be live after Undelete")
	}
	if _, err := store.Read(context.Background(), "db1", "A", verifier); err != nil {
		t.Errorf("Read after Undelete: %v", err)
	}
}

// TestUndelete_AlreadyLive_NoOp: undeleting a manifest that was
// never tombstoned is idempotent — returns (false, nil), and the
// manifest remains live.
func TestUndelete_AlreadyLive_NoOp(t *testing.T) {
	store, _, signer, verifier := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})

	restored, err := store.Undelete(context.Background(), "db1", "A")
	if err != nil {
		t.Fatalf("Undelete on live manifest: %v", err)
	}
	if restored {
		t.Errorf("Undelete returned restored=true for an already-live manifest; want false")
	}
	// Manifest still live.
	if _, err := store.Read(context.Background(), "db1", "A", verifier); err != nil {
		t.Errorf("Read after no-op Undelete: %v", err)
	}
}

// TestUndelete_DoubleUndelete_Idempotent: Undelete twice in a row.
// Second call sees the manifest is already live and returns
// (false, nil) cleanly.
func TestUndelete_DoubleUndelete_Idempotent(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	seedSampleChunks(t, sp) // chunks present → restorability pre-flight passes
	if err := store.SoftDelete(context.Background(), "db1", "A", "manual", "first"); err != nil {
		t.Fatal(err)
	}
	first, err := store.Undelete(context.Background(), "db1", "A")
	if err != nil {
		t.Fatalf("first Undelete: %v", err)
	}
	if !first {
		t.Errorf("first Undelete should report restored=true")
	}
	second, err := store.Undelete(context.Background(), "db1", "A")
	if err != nil {
		t.Errorf("second Undelete should be a no-op; got %v", err)
	}
	if second {
		t.Errorf("second Undelete should report restored=false")
	}
}

// TestUndelete_RejectsBadInput: required-field guards.
func TestUndelete_RejectsBadInput(t *testing.T) {
	store, _, _, _ := newStore(t)
	cases := []struct{ deployment, id string }{
		{"", "A"},
		{"db1", ""},
	}
	for _, c := range cases {
		if _, err := store.Undelete(context.Background(), c.deployment, c.id); err == nil {
			t.Errorf("expected error for deployment=%q id=%q", c.deployment, c.id)
		}
	}
}

// TestReadIncludingTombstoned_LiveSameAsRead: live manifests
// round-trip through ReadIncludingTombstoned identically to
// Read — same body, Tombstoned=false, no error. Callers can
// use it uniformly without a pre-flight live/dead check.
func TestReadIncludingTombstoned_LiveSameAsRead(t *testing.T) {
	store, _, signer, verifier := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	m1, err := store.Read(context.Background(), "db1", "A", verifier)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	m2, dead, err := store.ReadIncludingTombstoned(context.Background(), "db1", "A", verifier)
	if err != nil {
		t.Fatalf("ReadIncludingTombstoned: %v", err)
	}
	if dead {
		t.Errorf("live manifest should not be Tombstoned")
	}
	if m1.BackupID != m2.BackupID || m1.Deployment != m2.Deployment {
		t.Errorf("manifests differ: read=%v, readIncluding=%v", m1, m2)
	}
}

// TestReadIncludingTombstoned_TombstonedSurfacesBody: a tombstoned
// manifest's body is returned with Tombstoned=true (rather than
// the ErrTombstoned that Read returns). Operators inspecting an
// undelete candidate need the manifest body to make the decision.
func TestReadIncludingTombstoned_TombstonedSurfacesBody(t *testing.T) {
	store, _, signer, verifier := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	if err := store.SoftDelete(context.Background(), "db1", "A", "manual", "test"); err != nil {
		t.Fatal(err)
	}
	// Read refuses with ErrTombstoned.
	if _, err := store.Read(context.Background(), "db1", "A", verifier); err == nil {
		t.Errorf("Read should refuse a tombstoned manifest")
	}
	// ReadIncludingTombstoned surfaces it.
	m, dead, err := store.ReadIncludingTombstoned(context.Background(), "db1", "A", verifier)
	if err != nil {
		t.Fatalf("ReadIncludingTombstoned: %v", err)
	}
	if !dead {
		t.Errorf("Tombstoned should be true")
	}
	if m == nil || m.BackupID != "A" {
		t.Errorf("manifest body missing or wrong: %+v", m)
	}
}

// TestReadIncludingTombstoned_NotFound: a manifest that simply
// doesn't exist still returns storage.ErrNotFound — same as Read.
// The tombstone bypass doesn't fabricate manifests.
func TestReadIncludingTombstoned_NotFound(t *testing.T) {
	store, _, _, verifier := newStore(t)
	_, _, err := store.ReadIncludingTombstoned(context.Background(), "db1", "nope", verifier)
	if err == nil {
		t.Fatal("expected ErrNotFound for non-existent manifest")
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("expected storage.ErrNotFound; got %v", err)
	}
}

// TestReadIncludingTombstoned_RejectsBadInput: required-field
// guards.
func TestReadIncludingTombstoned_RejectsBadInput(t *testing.T) {
	store, _, _, verifier := newStore(t)
	cases := []struct{ deployment, id string }{
		{"", "A"},
		{"db1", ""},
	}
	for _, c := range cases {
		if _, _, err := store.ReadIncludingTombstoned(context.Background(), c.deployment, c.id, verifier); err == nil {
			t.Errorf("expected error for deployment=%q id=%q", c.deployment, c.id)
		}
	}
}

// TestReadTombstone_RoundTrip: SoftDelete writes the tombstone
// body; ReadTombstone parses it back with Reason / Policy /
// TombstonedAt / Schema intact.
func TestReadTombstone_RoundTrip(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	if err := store.SoftDelete(context.Background(), "db1", "A", "manual", "operator-error"); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	tomb, err := store.ReadTombstone(context.Background(), "db1", "A")
	if err != nil {
		t.Fatalf("ReadTombstone: %v", err)
	}
	if tomb == nil {
		t.Fatal("ReadTombstone returned nil")
	}
	if tomb.BackupID != "A" || tomb.Deployment != "db1" {
		t.Errorf("tomb = %+v, want BackupID=A Deployment=db1", tomb)
	}
	if tomb.Policy != "manual" {
		t.Errorf("tomb.Policy = %q, want manual", tomb.Policy)
	}
	if tomb.Reason != "operator-error" {
		t.Errorf("tomb.Reason = %q, want operator-error", tomb.Reason)
	}
	if tomb.Schema != backup.TombstoneSchema {
		t.Errorf("tomb.Schema = %q, want %q", tomb.Schema, backup.TombstoneSchema)
	}
	if tomb.TombstonedAt.IsZero() {
		t.Errorf("TombstonedAt should be set")
	}
}

// TestReadTombstone_NotPresent: a live manifest's ReadTombstone
// returns ErrNotFound (the storage-plugin contract — no marker
// means no tombstone). Callers can branch on errors.Is for the
// "not deleted" case without misclassifying it as a real error.
func TestReadTombstone_NotPresent(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	_, err := store.ReadTombstone(context.Background(), "db1", "A")
	if err == nil {
		t.Fatal("ReadTombstone on live manifest should error")
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("expected storage.ErrNotFound; got %v", err)
	}
}

// TestReadTombstone_RejectsBadInput: required-field guards.
func TestReadTombstone_RejectsBadInput(t *testing.T) {
	store, _, _, _ := newStore(t)
	cases := []struct{ deployment, id string }{
		{"", "A"},
		{"db1", ""},
	}
	for _, c := range cases {
		if _, err := store.ReadTombstone(context.Background(), c.deployment, c.id); err == nil {
			t.Errorf("expected error for deployment=%q id=%q", c.deployment, c.id)
		}
	}
}

// TestListIncludingTombstoned_YieldsBoth: List filters out
// tombstoned manifests; ListIncludingTombstoned surfaces them
// alongside live ones with Tombstoned set correctly.
func TestListIncludingTombstoned_YieldsBoth(t *testing.T) {
	store, _, signer, verifier := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "live-A", btype: backup.BackupTypeFull},
		{id: "dead-B", btype: backup.BackupTypeFull},
	})
	if err := store.SoftDelete(context.Background(), "db1", "dead-B", "manual", "test"); err != nil {
		t.Fatal(err)
	}
	// Default List: only live-A.
	live := []string{}
	for m, err := range store.List(context.Background(), "db1", verifier) {
		if err != nil {
			t.Fatal(err)
		}
		live = append(live, m.BackupID)
	}
	if len(live) != 1 || live[0] != "live-A" {
		t.Errorf("List = %v, want [live-A]", live)
	}
	// ListIncludingTombstoned: both, with Tombstoned flagged.
	seen := map[string]bool{}
	for entry, err := range store.ListIncludingTombstoned(context.Background(), "db1", verifier) {
		if err != nil {
			t.Fatal(err)
		}
		seen[entry.Manifest.BackupID] = entry.Tombstoned
	}
	if len(seen) != 2 {
		t.Errorf("ListIncludingTombstoned saw %d, want 2: %v", len(seen), seen)
	}
	if seen["live-A"] {
		t.Errorf("live-A should not be Tombstoned")
	}
	if !seen["dead-B"] {
		t.Errorf("dead-B should be Tombstoned")
	}
}

// TestUndelete_RestoresChainAfterCascade: cascaded chain → undelete
// each manifest individually. After all undeletes, the whole chain
// is restorable. Operationally this is the "I cascaded too
// aggressively, give me back B and C" recovery path.
func TestUndelete_RestoresChainAfterCascade(t *testing.T) {
	store, sp, signer, verifier := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
		{id: "C", parent: "B", btype: backup.BackupTypeIncremental},
	})
	seedSampleChunks(t, sp) // every link shares sampleManifest's chunks → present
	deleted, err := store.SoftDeleteCascade(context.Background(), "db1", "A", "manual", "oops")
	if err != nil {
		t.Fatalf("SoftDeleteCascade: %v", err)
	}
	if len(deleted) != 3 {
		t.Fatalf("cascade deleted %d, want 3", len(deleted))
	}
	// Undelete in any order — there's no chain-protection on the
	// resurrection direction (un-tombstoning doesn't break invariants).
	for _, id := range []string{"A", "B", "C"} {
		restored, err := store.Undelete(context.Background(), "db1", id)
		if err != nil {
			t.Fatalf("Undelete %s: %v", id, err)
		}
		if !restored {
			t.Errorf("Undelete %s reported restored=false", id)
		}
	}
	// All three live again.
	for _, id := range []string{"A", "B", "C"} {
		if _, err := store.Read(context.Background(), "db1", id, verifier); err != nil {
			t.Errorf("Read %s after undelete: %v", id, err)
		}
	}
}

// TestUndelete_RefusesWhenChunksMissing pins the restorability
// pre-flight: a tombstoned backup whose chunks have been swept must
// NOT be resurrected. Undelete fails closed with
// ErrUndeleteChunksMissing and LEAVES THE TOMBSTONE IN PLACE, so the
// operator never gets back a healthy-looking but un-restorable
// backup. (Regression for data-loss path #2.)
func TestUndelete_RefusesWhenChunksMissing(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	// Deliberately do NOT seed the sample chunks — they are "missing"
	// exactly as if `repo gc --apply` had reclaimed them.
	if err := store.SoftDelete(context.Background(), "db1", "A", "manual", "test"); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}

	restored, err := store.Undelete(context.Background(), "db1", "A")
	if err == nil {
		t.Fatal("Undelete should refuse when chunks are missing; got nil error")
	}
	if restored {
		t.Error("Undelete reported restored=true despite missing chunks")
	}
	if !errors.Is(err, backup.ErrUndeleteChunksMissing) {
		t.Errorf("error should match ErrUndeleteChunksMissing; got %v", err)
	}
	var cm *backup.UndeleteChunksMissingError
	if !errors.As(err, &cm) {
		t.Fatalf("error should be *UndeleteChunksMissingError; got %T", err)
	}
	if len(cm.Missing) == 0 {
		t.Error("UndeleteChunksMissingError.Missing should list the absent chunk hashes")
	}
	// A refused undelete is a no-op, not a partial mutation: the
	// tombstone MUST still be present.
	dead, err := store.IsTombstoned(context.Background(), "db1", "A")
	if err != nil {
		t.Fatal(err)
	}
	if !dead {
		t.Error("manifest should still be tombstoned after a refused Undelete")
	}
}

// TestUndeleteForce_BypassesChunkCheck: the forensic escape hatch.
// UndeleteForce removes the tombstone even when chunks are gone, for
// an operator who wants the metadata back with eyes open.
func TestUndeleteForce_BypassesChunkCheck(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	// Chunks intentionally absent (as in the refusal test above).
	if err := store.SoftDelete(context.Background(), "db1", "A", "manual", "test"); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}

	restored, err := store.UndeleteForce(context.Background(), "db1", "A")
	if err != nil {
		t.Fatalf("UndeleteForce: %v", err)
	}
	if !restored {
		t.Error("UndeleteForce should report restored=true")
	}
	dead, err := store.IsTombstoned(context.Background(), "db1", "A")
	if err != nil {
		t.Fatal(err)
	}
	if dead {
		t.Error("manifest should be live after UndeleteForce")
	}
}
