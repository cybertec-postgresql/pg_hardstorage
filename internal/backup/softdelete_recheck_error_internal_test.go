package backup

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// failHoldGetSP fails every Get of a hold marker after the first
// `allow` succeed — i.e. the post-write hold RE-check cannot run.
type failHoldGetSP struct {
	storage.StoragePlugin
	allow atomic.Int32
}

func (s *failHoldGetSP) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if strings.HasSuffix(key, holdSuffix) {
		if s.allow.Add(-1) < 0 {
			return nil, errors.New("backend: 500 internal error")
		}
	}
	return s.StoragePlugin.Get(ctx, key)
}

// SoftDelete (and the batch/cascade forms) returned an error when a
// post-write re-check could not run, but KEPT the tombstone: the caller
// saw "delete failed" while the backup had in fact vanished from List.
// A re-check that cannot prove the delete safe must undo it.
func TestSoftDelete_RecheckErrorRollsBackTombstone(t *testing.T) {
	for _, form := range []string{"single", "batch", "cascade"} {
		t.Run(form, func(t *testing.T) {
			_, base, signer, _ := raceWorld(t)
			sp := &failHoldGetSP{StoragePlugin: base}
			store := NewManifestStore(sp)
			m := commitRaceManifest(t, store, base, signer, "db1.full.X", []byte("x-chunk"))
			ctx := context.Background()

			sp.allow.Store(1) // pre-check OK, re-check fails
			var err error
			switch form {
			case "single":
				err = store.SoftDelete(ctx, "db1", m.BackupID, "manual", "t")
			case "batch":
				_, err = store.SoftDeleteBatch(ctx, "db1", []string{m.BackupID}, "p", "t")
			case "cascade":
				_, err = store.SoftDeleteCascade(ctx, "db1", m.BackupID, "p", "t")
			}
			if err == nil {
				t.Fatal("re-check failure not reported")
			}
			dead, terr := store.IsTombstoned(ctx, "db1", m.BackupID)
			if terr != nil || dead {
				t.Fatalf("delete reported as failed but the tombstone stayed (tombstoned=%v, %v): %v", dead, terr, err)
			}
			if !strings.Contains(err.Error(), "NOT applied") {
				t.Errorf("error must say the delete was not applied: %v", err)
			}
		})
	}
}
