package backup_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// holdInjectingSP installs a legal hold (via a side store on the same
// underlying storage) the first time the tombstone key is written —
// simulating a `hold add` that races a `backup delete`: the hold lands
// AFTER SoftDelete's pre-check but during the tombstone write.
type holdInjectingSP struct {
	storage.StoragePlugin
	tombstoneKey string
	inject       func()
	once         sync.Once
}

// Put injects the hold right after SoftDelete writes the tombstone's tmp
// body but BEFORE the rename makes the tombstone visible — i.e. inside the
// pre-check→re-check window (the genuine race). The hold therefore lands
// (PutHold's own tombstone guard sees no durable tombstone yet), and
// SoftDelete's post-tombstone hold re-check then catches it and rolls the
// tombstone back. (Injecting AFTER the rename instead makes PutHold refuse
// outright via its tombstone guard — also safe; covered separately by
// TestPutHold_RefusesTombstonedBackup.)
func (s *holdInjectingSP) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	res, err := s.StoragePlugin.Put(ctx, key, r, opts)
	if err == nil && strings.HasPrefix(key, s.tombstoneKey+".tmp.") {
		s.once.Do(s.inject)
	}
	return res, err
}

// TestSoftDelete_HoldPlacedDuringDeleteRollsBack pins race-condition audit
// #1: a legal hold installed concurrently with a SoftDelete (after the
// pre-check, during the tombstone write) is caught by the post-tombstone
// hold re-check, which rolls the tombstone back and refuses — so a held
// backup is never silently tombstoned.
func TestSoftDelete_HoldPlacedDuringDeleteRollsBack(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	ctx := context.Background()

	m := sampleManifest()
	m.Deployment = "db1"
	m.BackupID = "db1.full.A"
	m.Type = backup.BackupTypeFull
	m.ParentBackupID = ""
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	wrapped := &holdInjectingSP{
		StoragePlugin: sp,
		tombstoneKey:  backup.TombstonePath(m.Deployment, m.BackupID),
		inject: func() {
			// Side store over the same raw storage — installs the hold
			// without recursing through the wrapper.
			_ = store.PutHold(ctx, m.Deployment, m.BackupID, "ops", "litigation hold")
		},
	}
	delStore := backup.NewManifestStore(wrapped)

	err := delStore.SoftDelete(ctx, m.Deployment, m.BackupID, "manual", "routine")
	var held *backup.ManifestHeldError
	if !errors.As(err, &held) {
		t.Fatalf("SoftDelete should refuse a concurrently-held backup with *ManifestHeldError; got %T (%v)", err, err)
	}

	// The tombstone must have been rolled back: the backup is still live.
	if dead, derr := store.IsTombstoned(ctx, m.Deployment, m.BackupID); derr != nil {
		t.Fatal(derr)
	} else if dead {
		t.Error("backup must NOT be tombstoned after the hold re-check rolled it back")
	}
	// And the hold is intact.
	if h, herr := store.GetHold(ctx, m.Deployment, m.BackupID); herr != nil || h == nil {
		t.Errorf("hold should still be present; got (%v, %v)", h, herr)
	}
}

// holdSwapSP returns an EXPIRED hold body on the first Get of the hold key
// (PurgeExpiredHolds' snapshot read) and an ACTIVE one thereafter (the
// purge's re-read) — simulating a `hold add --until <future>` renewal that
// races the purge.
type holdSwapSP struct {
	storage.StoragePlugin
	holdKey string
	expired []byte
	active  []byte
	gets    atomic.Int32
}

func (s *holdSwapSP) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if key == s.holdKey {
		if s.gets.Add(1) == 1 {
			return io.NopCloser(bytes.NewReader(s.expired)), nil
		}
		return io.NopCloser(bytes.NewReader(s.active)), nil
	}
	return s.StoragePlugin.Get(ctx, key)
}

// TestPurgeExpiredHolds_SkipsRenewedHold pins race-condition audit #2: a
// hold that looked expired in the ListHolds snapshot but was RENEWED to
// active before the purge deletes it is re-read and kept, not removed.
func TestPurgeExpiredHolds_SkipsRenewedHold(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	ctx := context.Background()

	m := sampleManifest()
	m.Deployment = "db1"
	m.BackupID = "db1.full.A"
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// Write a real (expired) hold so ListHolds' List finds the key.
	past := time.Now().UTC().Add(-time.Hour)
	if err := store.PutHoldUntil(ctx, m.Deployment, m.BackupID, "ops", "temp", past); err != nil {
		t.Fatalf("put expired hold: %v", err)
	}

	future := time.Now().UTC().Add(24 * time.Hour)
	mustJSON := func(h backup.Hold) []byte {
		b, err := json.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	base := backup.Hold{Schema: backup.HoldSchema, BackupID: m.BackupID, Deployment: m.Deployment, HeldAt: past}
	expiredH, activeH := base, base
	expiredH.ExpiresAt = &past
	activeH.ExpiresAt = &future

	wrapped := &holdSwapSP{
		StoragePlugin: sp,
		holdKey:       backup.HoldPath(m.Deployment, m.BackupID),
		expired:       mustJSON(expiredH),
		active:        mustJSON(activeH),
	}
	purgeStore := backup.NewManifestStore(wrapped)

	removed, err := purgeStore.PurgeExpiredHolds(ctx, m.Deployment, false)
	if err != nil {
		t.Fatalf("PurgeExpiredHolds: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("a hold renewed between snapshot and delete must NOT be purged; removed %d", len(removed))
	}
	// The hold marker must still exist (RemoveHold was not called).
	if h, herr := store.GetHold(ctx, m.Deployment, m.BackupID); herr != nil || h == nil {
		t.Errorf("renewed hold must survive the purge; got (%v, %v)", h, herr)
	}
}

// TestPurgeExpiredHolds_RemovesStillExpired: the happy path still works —
// a hold that is expired on re-read is removed.
func TestPurgeExpiredHolds_RemovesStillExpired(t *testing.T) {
	store, _, signer, _ := newStore(t)
	ctx := context.Background()

	m := sampleManifest()
	m.Deployment = "db1"
	m.BackupID = "db1.full.A"
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	if err := store.PutHoldUntil(ctx, m.Deployment, m.BackupID, "ops", "temp", past); err != nil {
		t.Fatalf("put expired hold: %v", err)
	}

	removed, err := store.PurgeExpiredHolds(ctx, m.Deployment, false)
	if err != nil {
		t.Fatalf("PurgeExpiredHolds: %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("a genuinely-expired hold should be purged; removed %d", len(removed))
	}
	if _, herr := store.GetHold(ctx, m.Deployment, m.BackupID); !errors.Is(herr, storage.ErrNotFound) {
		t.Errorf("expired hold should be gone; got %v", herr)
	}
}

// TestPutHold_RefusesTombstonedBackup pins the core fix: a hold on an
// already-soft-deleted backup is refused. The primary manifest.json
// survives a SoftDelete (the tombstone is a sibling marker), so the old
// existence Stat passed and the hold was silently installed on a backup
// GC reaps after grace — a legal hold protecting nothing.
func TestPutHold_RefusesTombstonedBackup(t *testing.T) {
	store, _, signer, _ := newStore(t)
	ctx := context.Background()
	m := sampleManifest()
	m.Deployment = "db1"
	m.BackupID = "db1.full.A"
	m.Type = backup.BackupTypeFull
	m.ParentBackupID = ""
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := store.SoftDelete(ctx, m.Deployment, m.BackupID, "manual", "routine"); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	err := store.PutHold(ctx, m.Deployment, m.BackupID, "ops", "litigation hold")
	if !errors.Is(err, backup.ErrManifestTombstoned) {
		t.Fatalf("PutHold on a tombstoned backup must refuse with ErrManifestTombstoned; got %T (%v)", err, err)
	}
	if held, herr := store.IsHeld(ctx, m.Deployment, m.BackupID); herr != nil || held {
		t.Errorf("no hold marker should remain after a refused PutHold; got held=%v err=%v", held, herr)
	}
}

// tombstoneInjectingSP installs a tombstone (raw, simulating a concurrent
// SoftDelete that already committed its marker) the first time the hold
// marker is written — exercising PutHold's post-write tombstone re-check.
type tombstoneInjectingSP struct {
	storage.StoragePlugin
	holdKey      string
	tombstoneKey string
	once         sync.Once
}

func (s *tombstoneInjectingSP) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	res, err := s.StoragePlugin.Put(ctx, key, r, opts)
	if err == nil && key == s.holdKey {
		s.once.Do(func() {
			_, _ = s.StoragePlugin.Put(ctx, s.tombstoneKey, bytes.NewReader([]byte("{}")),
				storage.PutOptions{ContentLength: 2})
		})
	}
	return res, err
}

// TestPutHold_TombstoneInstalledDuringHold_RollsBack pins PutHold's
// write-then-verify: a SoftDelete that tombstones the backup in the window
// between PutHold's pre-check and its write must be caught by the
// post-write re-check, which removes the just-written hold and refuses —
// so the chain never ends up both held and tombstoned.
func TestPutHold_TombstoneInstalledDuringHold_RollsBack(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	ctx := context.Background()
	m := sampleManifest()
	m.Deployment = "db1"
	m.BackupID = "db1.full.A"
	m.Type = backup.BackupTypeFull
	m.ParentBackupID = ""
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	wrapped := &tombstoneInjectingSP{
		StoragePlugin: sp,
		holdKey:       backup.HoldPath(m.Deployment, m.BackupID),
		tombstoneKey:  backup.TombstonePath(m.Deployment, m.BackupID),
	}
	holdStore := backup.NewManifestStore(wrapped)

	err := holdStore.PutHold(ctx, m.Deployment, m.BackupID, "ops", "litigation hold")
	if !errors.Is(err, backup.ErrManifestTombstoned) {
		t.Fatalf("PutHold racing a tombstone must refuse with ErrManifestTombstoned; got %T (%v)", err, err)
	}
	if held, herr := store.IsHeld(ctx, m.Deployment, m.BackupID); herr != nil || held {
		t.Errorf("the just-written hold must be rolled back when the post-check finds a tombstone; got held=%v err=%v", held, herr)
	}
}
