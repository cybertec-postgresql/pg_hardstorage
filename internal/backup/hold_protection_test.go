package backup_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// TestSoftDelete_RefusesHeld: SoftDelete on a held manifest
// returns *ManifestHeldError carrying the holder + reason from
// the marker. The structured error is what the CLI maps to
// `conflict.manifest_held`.
func TestSoftDelete_RefusesHeld(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	if err := store.PutHold(context.Background(), "db1", "A",
		"ops@acme.com", "GDPR-art-17-#1234"); err != nil {
		t.Fatalf("PutHold: %v", err)
	}
	err := store.SoftDelete(context.Background(), "db1", "A", "manual", "test")
	if err == nil {
		t.Fatal("expected SoftDelete to refuse a held manifest")
	}
	var heldErr *backup.ManifestHeldError
	if !errors.As(err, &heldErr) {
		t.Fatalf("expected *ManifestHeldError; got %T: %v", err, err)
	}
	if heldErr.Holder != "ops@acme.com" {
		t.Errorf("Holder = %q, want ops@acme.com", heldErr.Holder)
	}
	if heldErr.Reason != "GDPR-art-17-#1234" {
		t.Errorf("Reason = %q, want GDPR-art-17-#1234", heldErr.Reason)
	}
	if heldErr.HeldAt.IsZero() {
		t.Errorf("HeldAt should be populated")
	}
	// Sentinel matches.
	if !errors.Is(err, backup.ErrManifestHeld) {
		t.Errorf("errors.Is(err, ErrManifestHeld) should be true")
	}
	// And the manifest is NOT tombstoned.
	if dead, _ := store.IsTombstoned(context.Background(), "db1", "A"); dead {
		t.Errorf("held manifest should not be tombstoned after refused SoftDelete")
	}
}

// TestSoftDelete_AllowsAfterHoldRemoved: removing the hold
// re-enables deletion. Round-trip protection.
func TestSoftDelete_AllowsAfterHoldRemoved(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	if err := store.PutHold(context.Background(), "db1", "A", "ops", "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.SoftDelete(context.Background(), "db1", "A", "manual", "first"); err == nil {
		t.Fatal("expected refusal while held")
	}
	if err := store.RemoveHold(context.Background(), "db1", "A"); err != nil {
		t.Fatal(err)
	}
	if err := store.SoftDelete(context.Background(), "db1", "A", "manual", "after-release"); err != nil {
		t.Errorf("SoftDelete after RemoveHold: %v", err)
	}
}

// TestSoftDeleteCascade_RefusesIfAnyHeld: cascade refuses
// up-front if ANY link in the chain is held — partial cascades
// would tear the chain. The error lists every held link so
// the operator fixes them all in one pass.
func TestSoftDeleteCascade_RefusesIfAnyHeld(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
		{id: "C", parent: "B", btype: backup.BackupTypeIncremental},
	})
	// Hold the middle link.
	if err := store.PutHold(context.Background(), "db1", "B", "compliance", "litigation-hold"); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.SoftDeleteCascade(context.Background(), "db1", "A", "manual", "cascade")
	if err == nil {
		t.Fatal("expected cascade to refuse")
	}
	if len(deleted) != 0 {
		t.Errorf("cascade should refuse up-front; deleted=%v", deleted)
	}
	var heldErr *backup.ChainHasHeldLinksError
	if !errors.As(err, &heldErr) {
		t.Fatalf("expected *ChainHasHeldLinksError; got %T", err)
	}
	if len(heldErr.Held) != 1 || heldErr.Held[0].BackupID != "B" {
		t.Errorf("Held = %+v, want [B]", heldErr.Held)
	}
	if heldErr.Held[0].Holder != "compliance" {
		t.Errorf("Held[0].Holder = %q", heldErr.Held[0].Holder)
	}
	if !errors.Is(err, backup.ErrChainHasHeldLinks) {
		t.Errorf("errors.Is should match ErrChainHasHeldLinks")
	}
	// No link should be tombstoned (refusal is up-front).
	for _, id := range []string{"A", "B", "C"} {
		if dead, _ := store.IsTombstoned(context.Background(), "db1", id); dead {
			t.Errorf("%s should NOT be tombstoned after refused cascade", id)
		}
	}
}

// TestSoftDeleteCascade_RefusesIfRootHeld: a hold on the
// cascade root alone is enough to refuse; root + descendants
// are checked uniformly.
func TestSoftDeleteCascade_RefusesIfRootHeld(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
	})
	if err := store.PutHold(context.Background(), "db1", "A", "ops", "root-protection"); err != nil {
		t.Fatal(err)
	}
	_, err := store.SoftDeleteCascade(context.Background(), "db1", "A", "manual", "cascade")
	if err == nil {
		t.Fatal("expected cascade to refuse on held root")
	}
	var heldErr *backup.ChainHasHeldLinksError
	if !errors.As(err, &heldErr) {
		t.Fatalf("expected *ChainHasHeldLinksError; got %T", err)
	}
	ids := []string{}
	for _, l := range heldErr.Held {
		ids = append(ids, l.BackupID)
	}
	if len(ids) != 1 || ids[0] != "A" {
		t.Errorf("Held IDs = %v, want [A]", ids)
	}
}

// TestSoftDeleteCascade_ListsAllHeldLinks: when MULTIPLE links
// are held, every one is reported. Operator fixes them all in
// one pass.
func TestSoftDeleteCascade_ListsAllHeldLinks(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
		{id: "C", parent: "B", btype: backup.BackupTypeIncremental},
	})
	for _, id := range []string{"A", "C"} {
		if err := store.PutHold(context.Background(), "db1", id, "ops", "test"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := store.SoftDeleteCascade(context.Background(), "db1", "A", "manual", "cascade")
	if err == nil {
		t.Fatal("expected refusal")
	}
	var heldErr *backup.ChainHasHeldLinksError
	if !errors.As(err, &heldErr) {
		t.Fatalf("expected *ChainHasHeldLinksError; got %T", err)
	}
	heldIDs := []string{}
	for _, l := range heldErr.Held {
		heldIDs = append(heldIDs, l.BackupID)
	}
	// Order is whatever the chain walk produces — assert set.
	got := strings.Join(heldIDs, ",")
	if !(strings.Contains(got, "A") && strings.Contains(got, "C")) {
		t.Errorf("expected both A and C in held list; got %v", heldIDs)
	}
	if strings.Contains(got, "B") {
		t.Errorf("B is not held; should not appear: %v", heldIDs)
	}
}

// TestManifestHeldError_Message: error message includes the
// backup ID + holder + reason for operator clarity.
func TestManifestHeldError_Message(t *testing.T) {
	store, _, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
	})
	if err := store.PutHold(context.Background(), "db1", "A",
		"jane@acme.com", "investigation-pending"); err != nil {
		t.Fatal(err)
	}
	err := store.SoftDelete(context.Background(), "db1", "A", "manual", "test")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{
		"db1/A",
		"legal hold",
		"jane@acme.com",
		"investigation-pending",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q:\n%s", want, msg)
		}
	}
}
