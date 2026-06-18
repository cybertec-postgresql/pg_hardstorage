package backup_test

import (
	"context"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// TestPurgeExpiredHolds_RemovesPastBoundedOnly: of three holds
// (indefinite, future-bounded, past-bounded), only the past-
// bounded marker is removed. Indefinite holds (legal-hold
// default) and active bounded holds stay put.
func TestPurgeExpiredHolds_RemovesPastBoundedOnly(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "indef", btype: backup.BackupTypeFull},
		{id: "active", btype: backup.BackupTypeFull},
		{id: "expired", btype: backup.BackupTypeFull},
	})
	if err := store.PutHold(context.Background(), "db1", "indef",
		"compliance", "GDPR-art-17"); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(time.Hour)
	if err := store.PutHoldUntil(context.Background(), "db1", "active",
		"ops", "active-window", future); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	if err := store.PutHoldUntil(context.Background(), "db1", "expired",
		"old-debug", "stale", past); err != nil {
		t.Fatal(err)
	}

	removed, err := store.PurgeExpiredHolds(context.Background(), "db1", false)
	if err != nil {
		t.Fatalf("PurgeExpiredHolds: %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("removed %d, want 1", len(removed))
	}
	if removed[0].BackupID != "expired" {
		t.Errorf("removed[0].BackupID = %q, want expired", removed[0].BackupID)
	}
	if removed[0].Holder != "old-debug" || removed[0].Reason != "stale" {
		t.Errorf("metadata mismatch: %+v", removed[0])
	}
	if !removed[0].ExpiredAt.Equal(past) {
		t.Errorf("ExpiredAt = %v, want %v", removed[0].ExpiredAt, past)
	}

	// Confirm survivors.
	for _, id := range []string{"indef", "active"} {
		held, err := store.IsHeld(context.Background(), "db1", id)
		if err != nil {
			t.Fatal(err)
		}
		if !held {
			t.Errorf("%s should still be held after purge", id)
		}
	}
	// Confirm removed.
	dead, err := store.IsHeld(context.Background(), "db1", "expired")
	if err != nil {
		t.Fatal(err)
	}
	if dead {
		t.Errorf("expired marker should be gone")
	}
}

// TestPurgeExpiredHolds_DryRun_NoMutation: dry-run identifies
// expired markers without removing any. Re-running real
// returns the same list.
func TestPurgeExpiredHolds_DryRun_NoMutation(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "expired1", btype: backup.BackupTypeFull},
		{id: "expired2", btype: backup.BackupTypeFull},
	})
	past := time.Now().UTC().Add(-time.Hour)
	for _, id := range []string{"expired1", "expired2"} {
		if err := store.PutHoldUntil(context.Background(), "db1", id,
			"ops", "stale", past); err != nil {
			t.Fatal(err)
		}
	}

	preview, err := store.PurgeExpiredHolds(context.Background(), "db1", true)
	if err != nil {
		t.Fatalf("dry-run PurgeExpiredHolds: %v", err)
	}
	if len(preview) != 2 {
		t.Fatalf("preview returned %d, want 2", len(preview))
	}
	// Markers must still be on disk.
	for _, id := range []string{"expired1", "expired2"} {
		held, err := store.IsHeld(context.Background(), "db1", id)
		if err != nil {
			t.Fatal(err)
		}
		if !held {
			t.Errorf("%s should still have marker after dry-run", id)
		}
	}
	// Real run finishes the job.
	removed, err := store.PurgeExpiredHolds(context.Background(), "db1", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Errorf("real run removed %d, want 2", len(removed))
	}
}

// TestPurgeExpiredHolds_NoExpired_ReturnsEmpty: a deployment
// with only indefinite + active holds returns an empty slice
// cleanly (no error).
func TestPurgeExpiredHolds_NoExpired_ReturnsEmpty(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	if err := store.PutHold(context.Background(), "db1", "A", "ops", "indef"); err != nil {
		t.Fatal(err)
	}
	removed, err := store.PurgeExpiredHolds(context.Background(), "db1", false)
	if err != nil {
		t.Fatalf("PurgeExpiredHolds: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("expected empty; got %v", removed)
	}
}

// TestPurgeExpiredHolds_FleetWide: scope="" walks every
// deployment's holds.
func TestPurgeExpiredHolds_FleetWide(t *testing.T) {
	store, _, signer, _ := newStore(t)
	for _, dep := range []string{"db1", "db2"} {
		commitChain(t, store, signer, dep, []chainLink{
			{id: "expired", btype: backup.BackupTypeFull},
		})
	}
	past := time.Now().UTC().Add(-time.Hour)
	for _, dep := range []string{"db1", "db2"} {
		if err := store.PutHoldUntil(context.Background(), dep, "expired",
			"ops", "test", past); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := store.PurgeExpiredHolds(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Errorf("fleet-wide purge removed %d, want 2", len(removed))
	}
	deps := map[string]bool{}
	for _, r := range removed {
		deps[r.Deployment] = true
	}
	if !deps["db1"] || !deps["db2"] {
		t.Errorf("expected both db1 and db2 in fleet-wide purge; got %v", deps)
	}
}

// TestPurgeExpiredHolds_Idempotent: re-running on a
// already-clean state is a no-op.
func TestPurgeExpiredHolds_Idempotent(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "expired", btype: backup.BackupTypeFull},
	})
	past := time.Now().UTC().Add(-time.Hour)
	if err := store.PutHoldUntil(context.Background(), "db1", "expired",
		"ops", "stale", past); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PurgeExpiredHolds(context.Background(), "db1", false); err != nil {
		t.Fatal(err)
	}
	// Second run — nothing left.
	removed, err := store.PurgeExpiredHolds(context.Background(), "db1", false)
	if err != nil {
		t.Errorf("idempotent re-run: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("re-run removed %d, want 0 (idempotent)", len(removed))
	}
}
