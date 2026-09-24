package backup_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

func TestPutHold_RefusesUnknownBackup(t *testing.T) {
	store, _, _, _ := newStore(t)
	err := store.PutHold(context.Background(), "ghost", "no-such-id", "ops", "test")
	if err == nil {
		t.Fatal("expected error for non-existent backup")
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("expected ErrNotFound; got %v", err)
	}
}

func TestPutHold_HappyPath(t *testing.T) {
	store, _, signer, _ := newStore(t)
	m := sampleManifest()
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutHold(context.Background(), m.Deployment, m.BackupID, "ops@acme", "GDPR test"); err != nil {
		t.Fatalf("PutHold: %v", err)
	}
	held, err := store.IsHeld(context.Background(), m.Deployment, m.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Error("IsHeld should be true after PutHold")
	}
	h, err := store.GetHold(context.Background(), m.Deployment, m.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	if h.Holder != "ops@acme" {
		t.Errorf("Holder = %q", h.Holder)
	}
	if h.Reason != "GDPR test" {
		t.Errorf("Reason = %q", h.Reason)
	}
	if h.Schema != backup.HoldSchema {
		t.Errorf("Schema = %q", h.Schema)
	}
}

func TestPutHold_PreservesHeldAt(t *testing.T) {
	// Editing a hold (e.g. updating Reason) MUST NOT reset HeldAt;
	// HeldAt is the audit-log artefact of when the hold was placed.
	store, _, signer, _ := newStore(t)
	m := sampleManifest()
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutHold(context.Background(), m.Deployment, m.BackupID, "alice", "first"); err != nil {
		t.Fatal(err)
	}
	h0, _ := store.GetHold(context.Background(), m.Deployment, m.BackupID)

	// A small sleep guarantees a different time.Now() if PutHold
	// were to (incorrectly) update HeldAt.
	time.Sleep(5 * time.Millisecond)

	if _, err := store.PutHoldWithOptions(context.Background(), m.Deployment, m.BackupID, backup.PutHoldOptions{
		Holder: "bob", Reason: "updated", Force: true}); err != nil {
		t.Fatal(err)
	}
	h1, _ := store.GetHold(context.Background(), m.Deployment, m.BackupID)

	if !h1.HeldAt.Equal(h0.HeldAt) {
		t.Errorf("HeldAt changed across edits: %v -> %v", h0.HeldAt, h1.HeldAt)
	}
	if h1.Holder != "bob" {
		t.Errorf("Holder should update to bob; got %q", h1.Holder)
	}
	if h1.Reason != "updated" {
		t.Errorf("Reason should update; got %q", h1.Reason)
	}
}

func TestRemoveHold_Idempotent(t *testing.T) {
	store, _, signer, _ := newStore(t)
	m := sampleManifest()
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	// Remove without ever placing — must succeed.
	if err := store.RemoveHold(context.Background(), m.Deployment, m.BackupID); err != nil {
		t.Errorf("RemoveHold (no prior hold): %v", err)
	}
	if err := store.PutHold(context.Background(), m.Deployment, m.BackupID, "", ""); err != nil {
		t.Fatal(err)
	}
	// Remove twice in a row — both must succeed.
	for i := 0; i < 2; i++ {
		if err := store.RemoveHold(context.Background(), m.Deployment, m.BackupID); err != nil {
			t.Errorf("RemoveHold iter %d: %v", i, err)
		}
	}
}

func TestListHolds_FleetWide(t *testing.T) {
	store, _, signer, _ := newStore(t)

	// Two manifests in db1, one in db2; hold the db1[0] and db2.
	for _, dep := range []string{"db1", "db2"} {
		m := sampleManifest()
		m.Deployment = dep
		m.BackupID = dep + "." + m.BackupID
		if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := store.PutHold(context.Background(), dep, m.BackupID, "ops", dep+"-reason"); err != nil {
			t.Fatal(err)
		}
	}

	holds, err := store.ListHolds(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 2 {
		t.Errorf("fleet-wide ListHolds = %d, want 2", len(holds))
	}

	scoped, err := store.ListHolds(context.Background(), "db1")
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 {
		t.Errorf("scoped ListHolds(db1) = %d, want 1", len(scoped))
	}
	if scoped[0].Deployment != "db1" {
		t.Errorf("scoped result has wrong deployment %q", scoped[0].Deployment)
	}
}

func TestIsHeld_Absent(t *testing.T) {
	store, _, _, _ := newStore(t)
	held, err := store.IsHeld(context.Background(), "db1", "no-id")
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Error("absent hold must report false")
	}
}

// `hold add` on a backup that already carried an indefinite legal hold
// silently overwrote it — a re-add with --until 1d (or by a different
// holder) turned a regulatory hold into one that expired tomorrow.
// PutHoldUntil must refuse to weaken an active hold; only an explicit
// Force replaces it, and it reports what it replaced.
func TestPutHold_RefusesWeakening(t *testing.T) {
	store, _, signer, _ := newStore(t)
	m := sampleManifest()
	ctx := context.Background()
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutHold(ctx, m.Deployment, m.BackupID, "legal", "litigation #7"); err != nil {
		t.Fatal(err)
	}
	soon := time.Now().UTC().Add(24 * time.Hour)
	for name, o := range map[string]backup.PutHoldOptions{
		"finite over indefinite": {Holder: "legal", Reason: "oops", ExpiresAt: soon},
		"different holder":       {Holder: "mallory", Reason: "mine now"},
	} {
		_, err := store.PutHoldWithOptions(ctx, m.Deployment, m.BackupID, o)
		if !errors.Is(err, backup.ErrHoldWeaken) {
			t.Errorf("%s: err=%v, want ErrHoldWeaken", name, err)
		}
		h, _ := store.GetHold(ctx, m.Deployment, m.BackupID)
		if h.ExpiresAt != nil || h.Holder != "legal" {
			t.Fatalf("%s: the indefinite legal hold was weakened: %+v", name, h)
		}
	}
	// Re-asserting the same indefinite hold is fine.
	if err := store.PutHold(ctx, m.Deployment, m.BackupID, "legal", "litigation #7 (ref)"); err != nil {
		t.Fatalf("same-holder indefinite re-add: %v", err)
	}
	prev, err := store.PutHoldWithOptions(ctx, m.Deployment, m.BackupID, backup.PutHoldOptions{
		Holder: "legal", Reason: "released early", ExpiresAt: soon, Force: true})
	if err != nil || prev == nil || prev.ExpiresAt != nil {
		t.Fatalf("forced replace: prev=%+v err=%v, want the indefinite hold back", prev, err)
	}
	// Finite → shorter finite is also a weakening; longer is an extension.
	if err := store.PutHoldUntil(ctx, m.Deployment, m.BackupID, "legal", "", soon.Add(-time.Hour)); !errors.Is(err, backup.ErrHoldWeaken) {
		t.Errorf("shorter expiry: err=%v, want ErrHoldWeaken", err)
	}
	if err := store.PutHoldUntil(ctx, m.Deployment, m.BackupID, "legal", "", soon.Add(time.Hour)); err != nil {
		t.Errorf("extension refused: %v", err)
	}
}
