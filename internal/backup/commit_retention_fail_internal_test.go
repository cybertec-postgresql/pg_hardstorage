package backup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

type failRetentionSP struct{ storage.StoragePlugin }

func (failRetentionSP) SetRetention(context.Context, string, time.Time, storage.WORMMode) error {
	return errors.New("object lock: AccessDenied")
}

// Commit returned an error when SetRetention failed AFTER the manifest
// was written, yet left that manifest live — unlocked on a WORM repo,
// with no replica and (for an incremental) no parent check — while the
// caller believed the backup had failed. The outcome must be consistent:
// a failed commit leaves no manifest behind.
func TestCommit_RetentionFailureRollsBackManifest(t *testing.T) {
	_, base, signer, _ := raceWorld(t)
	ctx := context.Background()
	m := commitRaceManifest(t, NewManifestStore(base), base, signer, "db1.full.seed", []byte("seed"))

	m2 := *m
	m2.BackupID = "db1.full.W"
	m2.Attestation = nil
	store := NewManifestStore(failRetentionSP{base})
	err := store.Commit(ctx, &m2, signer, CommitOptions{RetainUntil: time.Now().Add(24 * time.Hour)})
	if err == nil {
		t.Fatal("Commit reported success although the WORM lock failed")
	}
	if _, serr := base.Stat(ctx, PrimaryPath("db1", "db1.full.W")); !errors.Is(serr, storage.ErrNotFound) {
		t.Fatalf("Commit failed but left the (unlocked) manifest live: stat err=%v", serr)
	}
}
