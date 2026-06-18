package backup_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// flakyReplicaSP fails the first failN writes to the replica tmp key,
// simulating a transient backend blip on the redundancy copy while the
// primary commit succeeds.
type flakyReplicaSP struct {
	storage.StoragePlugin
	tmpPrefix string
	failN     int
	calls     atomic.Int32
}

func (s *flakyReplicaSP) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	if strings.HasPrefix(key, s.tmpPrefix) && int(s.calls.Add(1)) <= s.failN {
		return storage.PutResult{}, errors.New("transient replica blip")
	}
	return s.StoragePlugin.Put(ctx, key, r, opts)
}

func commitFixture(t *testing.T, failN int) (*backup.ManifestStore, storage.StoragePlugin, *backup.Signer, *backup.Manifest, *flakyReplicaSP) {
	t.Helper()
	_, sp, signer, _ := newStore(t)
	m := sampleManifest()
	m.Deployment = "db1"
	m.BackupID = "db1.full.A"
	m.Type = backup.BackupTypeFull
	m.ParentBackupID = ""
	flaky := &flakyReplicaSP{
		StoragePlugin: sp,
		tmpPrefix:     backup.ReplicaPath(m.BackupID) + ".tmp.",
		failN:         failN,
	}
	return backup.NewManifestStore(flaky), sp, signer, m, flaky
}

// TestCommit_ReplicaWriteRetriesTransientFailure pins the fix: a transient
// failure on the replica write is retried, so the redundancy copy lands
// rather than being silently stranded (nothing self-heals it afterward).
func TestCommit_ReplicaWriteRetriesTransientFailure(t *testing.T) {
	store, raw, signer, m, _ := commitFixture(t, 2) // fail twice, succeed on the 3rd attempt
	ctx := context.Background()

	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := raw.Stat(ctx, backup.ReplicaPath(m.BackupID)); err != nil {
		t.Errorf("replica must be present after the retry succeeded; Stat err = %v", err)
	}
}

// TestCommit_ReplicaWriteExhaustsRetriesNonFatal: when every retry fails,
// the primary commit still succeeds (replica is non-fatal redundancy) and
// the error is surfaced to OnReplicaError.
func TestCommit_ReplicaWriteExhaustsRetriesNonFatal(t *testing.T) {
	store, raw, signer, m, _ := commitFixture(t, 100) // every attempt fails
	ctx := context.Background()

	var replicaErr error
	err := store.Commit(ctx, m, signer, backup.CommitOptions{
		OnReplicaError: func(e error) { replicaErr = e },
	})
	if err != nil {
		t.Fatalf("Commit must succeed despite replica failure; got %v", err)
	}
	if replicaErr == nil {
		t.Error("OnReplicaError should have been invoked after exhausting retries")
	}
	if _, perr := raw.Stat(ctx, backup.PrimaryPath(m.Deployment, m.BackupID)); perr != nil {
		t.Errorf("primary must be committed; Stat err = %v", perr)
	}
	if _, rerr := raw.Stat(ctx, backup.ReplicaPath(m.BackupID)); !errors.Is(rerr, storage.ErrNotFound) {
		t.Errorf("replica should be absent after all retries failed; Stat err = %v", rerr)
	}
}
