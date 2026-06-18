package backup_test

import (
	"bytes"
	"context"
	stdio "io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// replicaVerifies reads the replica copy and checks it parses + verifies.
func replicaVerifies(t *testing.T, sp storage.StoragePlugin, verifier *backup.Verifier, backupID string) bool {
	t.Helper()
	rc, err := sp.Get(context.Background(), backup.ReplicaPath(backupID))
	if err != nil {
		return false
	}
	defer rc.Close()
	body, err := stdio.ReadAll(rc)
	if err != nil {
		return false
	}
	_, err = backup.ParseAndVerify(body, verifier)
	return err == nil
}

// TestEnsureReplica_RebuildsMissingFromPrimary pins the fix for
// data-loss path #3: Commit treats the replica write as best-effort, so
// a failed (or out-of-band-deleted) replica leaves the primary with no
// fallback. EnsureReplica rebuilds the missing replica from the
// verified primary, restoring the redundancy — and is idempotent.
func TestEnsureReplica_RebuildsMissingFromPrimary(t *testing.T) {
	store, sp, signer, verifier := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{{id: "A", btype: backup.BackupTypeFull}})

	// Simulate the replica having never been written / been deleted.
	if err := sp.Delete(context.Background(), backup.ReplicaPath("A")); err != nil {
		t.Fatal(err)
	}
	if replicaVerifies(t, sp, verifier, "A") {
		t.Fatal("replica should be gone before EnsureReplica")
	}

	rebuilt, err := store.EnsureReplica(context.Background(), "db1", "A", verifier, time.Time{}, "")
	if err != nil {
		t.Fatalf("EnsureReplica: %v", err)
	}
	if !rebuilt {
		t.Error("EnsureReplica should report rebuilt=true for a missing replica")
	}
	if !replicaVerifies(t, sp, verifier, "A") {
		t.Error("replica should exist and verify after EnsureReplica")
	}

	// Idempotent: a second call is a no-op.
	again, err := store.EnsureReplica(context.Background(), "db1", "A", verifier, time.Time{}, "")
	if err != nil {
		t.Fatalf("second EnsureReplica: %v", err)
	}
	if again {
		t.Error("second EnsureReplica should be a no-op (rebuilt=false)")
	}
}

// TestEnsureReplica_RebuildsCorruptReplica: a present-but-unverifiable
// replica is replaced from the verified primary.
func TestEnsureReplica_RebuildsCorruptReplica(t *testing.T) {
	store, sp, signer, verifier := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{{id: "A", btype: backup.BackupTypeFull}})

	garbage := []byte("}{ not a valid signed manifest")
	if _, err := sp.Put(context.Background(), backup.ReplicaPath("A"),
		bytes.NewReader(garbage), storage.PutOptions{ContentLength: int64(len(garbage))}); err != nil {
		t.Fatal(err)
	}
	if replicaVerifies(t, sp, verifier, "A") {
		t.Fatal("corrupt replica should not verify")
	}

	rebuilt, err := store.EnsureReplica(context.Background(), "db1", "A", verifier, time.Time{}, "")
	if err != nil {
		t.Fatalf("EnsureReplica: %v", err)
	}
	if !rebuilt {
		t.Error("EnsureReplica should rebuild a corrupt replica")
	}
	if !replicaVerifies(t, sp, verifier, "A") {
		t.Error("replica should verify after rebuild")
	}
}

// TestEnsureReplica_BothGone_Errors: with neither a good primary nor a
// good replica, EnsureReplica refuses (it won't manufacture a copy from
// nothing) — the manifest is genuinely unrecoverable.
func TestEnsureReplica_BothGone_Errors(t *testing.T) {
	store, sp, signer, verifier := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{{id: "A", btype: backup.BackupTypeFull}})
	_ = sp.Delete(context.Background(), backup.ReplicaPath("A"))
	_ = sp.Delete(context.Background(), backup.PrimaryPath("db1", "A"))

	if _, err := store.EnsureReplica(context.Background(), "db1", "A", verifier, time.Time{}, ""); err == nil {
		t.Fatal("EnsureReplica with both copies gone must error")
	}
}

// TestEnsureReplica_ConcurrentRebuildsAreSafe soaks the delete+rename
// rebuild window: many goroutines race to heal the same missing
// replica. Exactly the redundancy must result — a verifying replica —
// with no corruption, no panic, and no error (a loser of the rename
// race sees ErrAlreadyExists and reports rebuilt=false cleanly).
func TestEnsureReplica_ConcurrentRebuildsAreSafe(t *testing.T) {
	store, sp, signer, verifier := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{{id: "A", btype: backup.BackupTypeFull}})

	const iterations = 40
	const racers = 8
	for it := 0; it < iterations; it++ {
		if err := sp.Delete(context.Background(), backup.ReplicaPath("A")); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var rebuilds int64
		wg.Add(racers)
		for r := 0; r < racers; r++ {
			go func() {
				defer wg.Done()
				rebuilt, err := store.EnsureReplica(context.Background(), "db1", "A", verifier, time.Time{}, "")
				if err != nil {
					t.Errorf("iter %d: EnsureReplica: %v", it, err)
					return
				}
				if rebuilt {
					atomic.AddInt64(&rebuilds, 1)
				}
			}()
		}
		wg.Wait()
		// Whatever the interleaving, the replica must end up present
		// and verifying.
		if !replicaVerifies(t, sp, verifier, "A") {
			t.Fatalf("iter %d: replica must verify after concurrent rebuilds", it)
		}
		if rebuilds < 1 {
			t.Errorf("iter %d: at least one racer should report rebuilt=true", it)
		}
	}
}

// recordRetentionSP records SetRetention calls so a test can assert that a
// repair-time replica rebuild applies the repo's WORM lock.
type recordRetentionSP struct {
	storage.StoragePlugin
	called bool
	key    string
	until  time.Time
	mode   storage.WORMMode
}

func (s *recordRetentionSP) SetRetention(ctx context.Context, key string, until time.Time, mode storage.WORMMode) error {
	s.called = true
	s.key = key
	s.until = until
	s.mode = mode
	return s.StoragePlugin.SetRetention(ctx, key, until, mode)
}

// TestEnsureReplica_AppliesWORMRetention pins the fix: a rebuilt replica on
// a compliance repo must be WORM-locked like the commit-time copy, not left
// freely deletable.
func TestEnsureReplica_AppliesWORMRetention(t *testing.T) {
	store, sp, signer, verifier := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{{id: "A", btype: backup.BackupTypeFull}})
	if err := sp.Delete(context.Background(), backup.ReplicaPath("A")); err != nil {
		t.Fatal(err)
	}

	rec := &recordRetentionSP{StoragePlugin: sp}
	wormStore := backup.NewManifestStore(rec)
	until := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	rebuilt, err := wormStore.EnsureReplica(context.Background(), "db1", "A", verifier, until, storage.WORMCompliance)
	if err != nil {
		t.Fatalf("EnsureReplica: %v", err)
	}
	if !rebuilt {
		t.Fatal("expected rebuilt=true")
	}
	if !rec.called {
		t.Fatal("EnsureReplica must apply WORM retention to the rebuilt replica (compliance repo)")
	}
	if rec.key != backup.ReplicaPath("A") {
		t.Errorf("retention applied to %q, want replica key %q", rec.key, backup.ReplicaPath("A"))
	}
	if !rec.until.Equal(until) {
		t.Errorf("retention deadline = %v, want %v", rec.until, until)
	}
	if rec.mode != storage.WORMCompliance {
		t.Errorf("retention mode = %q, want compliance", rec.mode)
	}
}
