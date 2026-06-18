package backup_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// TestHold_ActiveAt: indefinite holds (ExpiresAt nil) are
// always active; bounded holds are active before ExpiresAt and
// inactive on/after.
func TestHold_ActiveAt(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	// Indefinite — always active.
	indef := &backup.Hold{Schema: backup.HoldSchema}
	if !indef.ActiveAt(now) {
		t.Errorf("indefinite hold should be active")
	}
	if !indef.ActiveAt(now.Add(100 * 365 * 24 * time.Hour)) {
		t.Errorf("indefinite hold should be active a century later")
	}

	// Bounded — active before, inactive on/after.
	exp := now.Add(time.Hour)
	bounded := &backup.Hold{Schema: backup.HoldSchema, ExpiresAt: &exp}
	if !bounded.ActiveAt(now) {
		t.Errorf("bounded hold should be active before expiry")
	}
	if !bounded.ActiveAt(exp.Add(-time.Nanosecond)) {
		t.Errorf("bounded hold should be active just before expiry")
	}
	if bounded.ActiveAt(exp) {
		t.Errorf("bounded hold should be inactive AT expiry")
	}
	if bounded.ActiveAt(exp.Add(time.Hour)) {
		t.Errorf("bounded hold should be inactive after expiry")
	}

	// Nil receiver guard.
	var nilHold *backup.Hold
	if nilHold.ActiveAt(now) {
		t.Errorf("nil hold should not be active")
	}
}

// TestPutHoldUntil_PersistsExpiresAt: PutHoldUntil writes
// ExpiresAt; GetHold reads it back round-trip.
func TestPutHoldUntil_PersistsExpiresAt(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	exp := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := store.PutHoldUntil(context.Background(), "db1", "A",
		"ops@acme.com", "audit-window", exp); err != nil {
		t.Fatalf("PutHoldUntil: %v", err)
	}
	h, err := store.GetHold(context.Background(), "db1", "A")
	if err != nil {
		t.Fatal(err)
	}
	if h.ExpiresAt == nil {
		t.Fatal("ExpiresAt should be set")
	}
	if !h.ExpiresAt.Equal(exp) {
		t.Errorf("ExpiresAt = %v, want %v", h.ExpiresAt, exp)
	}
}

// TestPutHoldUntil_ZeroTimeMeansIndefinite: passing the zero
// time.Time leaves ExpiresAt nil on the marker.
func TestPutHoldUntil_ZeroTimeMeansIndefinite(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	if err := store.PutHoldUntil(context.Background(), "db1", "A",
		"ops", "indefinite", time.Time{}); err != nil {
		t.Fatal(err)
	}
	h, err := store.GetHold(context.Background(), "db1", "A")
	if err != nil {
		t.Fatal(err)
	}
	if h.ExpiresAt != nil {
		t.Errorf("zero expiresAt should leave ExpiresAt nil; got %v", h.ExpiresAt)
	}
}

// TestSoftDelete_AllowsExpiredHold: a hold whose ExpiresAt has
// passed no longer protects the manifest. SoftDelete proceeds
// (the marker stays on disk for audit).
func TestSoftDelete_AllowsExpiredHold(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	// Set the hold's ExpiresAt to 1ms ago.
	expired := time.Now().UTC().Add(-time.Millisecond)
	if err := store.PutHoldUntil(context.Background(), "db1", "A",
		"ops", "test", expired); err != nil {
		t.Fatal(err)
	}
	if err := store.SoftDelete(context.Background(), "db1", "A", "manual", "after-expiry"); err != nil {
		t.Errorf("SoftDelete past expired hold: %v", err)
	}
	if dead, _ := store.IsTombstoned(context.Background(), "db1", "A"); !dead {
		t.Errorf("manifest should be tombstoned after SoftDelete past expired hold")
	}
}

// TestSoftDelete_RefusesActiveBoundedHold: a hold with a future
// ExpiresAt still blocks SoftDelete — the cascade error is
// surfaced same as for indefinite holds.
func TestSoftDelete_RefusesActiveBoundedHold(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	future := time.Now().UTC().Add(time.Hour)
	if err := store.PutHoldUntil(context.Background(), "db1", "A",
		"ops", "active", future); err != nil {
		t.Fatal(err)
	}
	err := store.SoftDelete(context.Background(), "db1", "A", "manual", "test")
	if err == nil {
		t.Fatal("expected refusal on active bounded hold")
	}
	if !errors.Is(err, backup.ErrManifestHeld) {
		t.Errorf("expected ErrManifestHeld; got %v", err)
	}
}

// TestSoftDeleteCascade_AllowsExpiredHoldsInChain: the cascade
// pre-flight skips expired holds. Operator can resurrect a
// chain-prune workflow that's been blocked by a stale
// debugging-window hold without manually removing markers.
func TestSoftDeleteCascade_AllowsExpiredHoldsInChain(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
		{id: "C", parent: "B", btype: backup.BackupTypeIncremental},
	})
	expired := time.Now().UTC().Add(-time.Hour)
	if err := store.PutHoldUntil(context.Background(), "db1", "B",
		"old-debug", "expired-debug-window", expired); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.SoftDeleteCascade(context.Background(), "db1", "A", "manual", "cascade")
	if err != nil {
		t.Fatalf("cascade past expired hold: %v", err)
	}
	if len(deleted) != 3 {
		t.Errorf("expected 3 deleted; got %v", deleted)
	}
}

// TestSoftDeleteCascade_RefusesActiveBoundedHold: a bounded
// hold with future ExpiresAt still blocks the cascade.
func TestSoftDeleteCascade_RefusesActiveBoundedHold(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
	})
	future := time.Now().UTC().Add(time.Hour)
	if err := store.PutHoldUntil(context.Background(), "db1", "B",
		"compliance", "active", future); err != nil {
		t.Fatal(err)
	}
	_, err := store.SoftDeleteCascade(context.Background(), "db1", "A", "manual", "cascade")
	if err == nil {
		t.Fatal("expected cascade refusal on active bounded hold")
	}
	if !errors.Is(err, backup.ErrChainHasHeldLinks) {
		t.Errorf("expected ErrChainHasHeldLinks; got %v", err)
	}
}

// TestIsActivelyHeld: returns true only for present + active
// holds. Expired/absent both return (false, nil).
func TestIsActivelyHeld(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "live", btype: backup.BackupTypeFull},
		{id: "active", btype: backup.BackupTypeFull},
		{id: "expired", btype: backup.BackupTypeFull},
		{id: "indef", btype: backup.BackupTypeFull},
	})
	future := time.Now().UTC().Add(time.Hour)
	past := time.Now().UTC().Add(-time.Hour)
	if err := store.PutHoldUntil(context.Background(), "db1", "active", "ops", "test", future); err != nil {
		t.Fatal(err)
	}
	if err := store.PutHoldUntil(context.Background(), "db1", "expired", "ops", "test", past); err != nil {
		t.Fatal(err)
	}
	if err := store.PutHold(context.Background(), "db1", "indef", "ops", "test"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		id   string
		want bool
	}{
		{"live", false},    // no hold marker
		{"active", true},   // bounded, future expiry
		{"expired", false}, // bounded, past expiry
		{"indef", true},    // indefinite
	}
	for _, c := range cases {
		got, err := store.IsActivelyHeld(context.Background(), "db1", c.id)
		if err != nil {
			t.Errorf("IsActivelyHeld(%s): %v", c.id, err)
			continue
		}
		if got != c.want {
			t.Errorf("IsActivelyHeld(%s) = %v, want %v", c.id, got, c.want)
		}
	}
}

// TestPutHoldUntil_OverwritePreservesHeldAt: an edit (e.g.
// extending the expiry) doesn't reset HeldAt — that's the
// audit-trail invariant.
func TestPutHoldUntil_OverwritePreservesHeldAt(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	if err := store.PutHold(context.Background(), "db1", "A", "ops", "first"); err != nil {
		t.Fatal(err)
	}
	first, err := store.GetHold(context.Background(), "db1", "A")
	if err != nil {
		t.Fatal(err)
	}
	originalHeldAt := first.HeldAt

	// Sleep briefly then update with --until.
	time.Sleep(10 * time.Millisecond)
	future := time.Now().UTC().Add(time.Hour)
	if err := store.PutHoldUntil(context.Background(), "db1", "A",
		"ops-2", "extended", future); err != nil {
		t.Fatal(err)
	}
	updated, err := store.GetHold(context.Background(), "db1", "A")
	if err != nil {
		t.Fatal(err)
	}
	if !updated.HeldAt.Equal(originalHeldAt) {
		t.Errorf("HeldAt should be preserved; original=%v updated=%v",
			originalHeldAt, updated.HeldAt)
	}
	if updated.Holder != "ops-2" {
		t.Errorf("Holder should be updated; got %q", updated.Holder)
	}
	if updated.ExpiresAt == nil {
		t.Errorf("ExpiresAt should be set after edit")
	}
}

// TestPutHoldUntil_OverwriteAtomicNoAbsentWindow: a hold edit
// that goes through PutHoldUntil's atomic-overwrite path
// (audit fix) MUST keep the marker continuously present.
//
// Pre-fix, the implementation did Delete(key) followed by
// RenameIfNotExists(tmp, key) — between those two calls the
// hold marker was ABSENT on disk, so a concurrent SoftDelete
// observing the missing marker bypassed legal-hold protection.
//
// We assert the post-fix invariant: at every moment between
// PutHold and PutHoldUntil's overwrite, IsHeld returns true.
// We can't directly assert "no race window" without a real
// concurrency interleaving, but we CAN assert the simpler
// invariant: a PutHoldUntil that's editing an existing hold
// never causes IsHeld to return false at any sample point.
func TestPutHoldUntil_OverwriteAtomicNoAbsentWindow(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	if err := store.PutHold(context.Background(), "db1", "A", "ops", "first"); err != nil {
		t.Fatal(err)
	}
	if held, err := store.IsHeld(context.Background(), "db1", "A"); err != nil || !held {
		t.Fatalf("post-PutHold: IsHeld = (%v, %v); want (true, nil)", held, err)
	}

	// Issue 50 sequential edits and check IsHeld between each.
	// Pre-fix, even sequential edits would never go through the
	// vulnerable Delete-then-Rename path because of the tmp+
	// RenameIfNotExists collision; the race only triggers under
	// concurrent edits.  But: post-fix the path is Put(key, body,
	// IfNotExists=false) which is atomic on every backend, so
	// sequential edits are also continuously held.  This sequence
	// guards against future regressions that re-introduce the
	// delete+rename pattern.
	for i := 0; i < 50; i++ {
		exp := time.Now().UTC().Add(time.Duration(i+1) * time.Hour)
		if err := store.PutHoldUntil(context.Background(), "db1", "A",
			"ops", "edit", exp); err != nil {
			t.Fatalf("edit %d: %v", i, err)
		}
		if held, err := store.IsHeld(context.Background(), "db1", "A"); err != nil || !held {
			t.Fatalf("post-edit %d: IsHeld = (%v, %v); want (true, nil)", i, held, err)
		}
	}
}

// TestPutHoldUntil_ConcurrentEdits_NoAbsentMarker: parallel
// PutHoldUntil calls against the same key all leave a hold
// marker present at every moment.  Pre-fix (delete+rename),
// goroutine B's Delete could fire between A's tmp-write and
// rename, leaving the key absent for a window B saw as "no
// hold to overwrite, install fresh."  Post-fix, every edit is
// an atomic overwrite — no goroutine can observe the marker
// missing.
func TestPutHoldUntil_ConcurrentEdits_NoAbsentMarker(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	if err := store.PutHold(context.Background(), "db1", "A", "ops", "seed"); err != nil {
		t.Fatal(err)
	}

	const writers = 4
	const editsEach = 25
	var wg = make(chan struct{}, writers)
	for w := 0; w < writers; w++ {
		wid := w
		go func() {
			for i := 0; i < editsEach; i++ {
				exp := time.Now().UTC().Add(time.Hour + time.Duration(wid*100+i)*time.Millisecond)
				_ = store.PutHoldUntil(context.Background(), "db1", "A",
					"ops", "concurrent", exp)
			}
			wg <- struct{}{}
		}()
	}
	// Reader: probe IsHeld continuously while the writers run.
	stop := make(chan struct{})
	probeErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				probeErr <- nil
				return
			default:
				held, err := store.IsHeld(context.Background(), "db1", "A")
				if err != nil {
					probeErr <- err
					return
				}
				if !held {
					probeErr <- errors.New("IsHeld returned false during concurrent edits — hold marker was absent for a window")
					return
				}
			}
		}
	}()
	for i := 0; i < writers; i++ {
		<-wg
	}
	close(stop)
	if err := <-probeErr; err != nil {
		t.Fatal(err)
	}
}
