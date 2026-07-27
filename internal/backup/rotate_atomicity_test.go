package backup_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// failingPutSP wraps a StoragePlugin and fails Put for keys matching
// a substring, simulating a transient storage error mid-rotation.
type failingPutSP struct {
	storage.StoragePlugin
	failSubstr string
	failed     bool
}

func (f *failingPutSP) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	if !f.failed && strings.Contains(key, f.failSubstr) && strings.HasSuffix(key, "/manifest.json") {
		f.failed = true
		return storage.PutResult{}, errors.New("injected transient storage failure")
	}
	return f.StoragePlugin.Put(ctx, key, r, opts)
}

// KEK rotation used to rewrite manifests as Put-tmp → DELETE original
// → RenameIfNotExists, and on a rename failure it "cleaned up" by
// deleting the tmp — destroying the only manifest copy at the primary
// key. The backup then vanished from List/retention and from GC's
// reference walk, so the next `repo gc --apply` reaped its chunks:
// permanent loss triggered by one flaky rename during routine key
// rotation. The rewrite must be a single atomic overwrite — after ANY
// failure a valid manifest body (old or new) must still exist at the
// primary key.
func TestRotateKEK_FailedRewriteNeverLosesTheManifest(t *testing.T) {
	w := setupRotateWorld(t)
	ctx := context.Background()
	oldKEK, newKEK := mkKEK(t), mkKEK(t)

	const backupID = "db1.full.20260430T120000Z.cccc"
	w.commitEncrypted(t, "db1", backupID, oldKEK, "local:default", 0)

	fsp := &failingPutSP{StoragePlugin: w.sp, failSubstr: backupID}
	res, err := backup.RotateKEK(ctx, fsp, backup.RotateKEKOptions{
		OldKEKRef: "local:default", OldKEK: oldKEK,
		NewKEKRef: "local:v2", NewKEK: newKEK,
		Signer: w.signer, Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("RotateKEK returned hard error: %v", err)
	}
	if res.Failed != 1 {
		t.Fatalf("Failed = %d, want 1 (the injected Put failure)", res.Failed)
	}

	// THE invariant: the manifest must still be present and readable
	// at its primary key — under either KEK ref, but never absent.
	m, rerr := w.store.Read(ctx, "db1", backupID, w.verifier)
	if rerr != nil {
		t.Fatalf("manifest LOST after failed rotation rewrite: %v — GC would reap this backup's chunks", rerr)
	}
	if m.Encryption == nil {
		t.Fatal("manifest survived but lost its encryption block")
	}

	// And a retry with a healthy plugin completes the rotation.
	res2, err := backup.RotateKEK(ctx, w.sp, backup.RotateKEKOptions{
		OldKEKRef: "local:default", OldKEK: oldKEK,
		NewKEKRef: "local:v2", NewKEK: newKEK,
		Signer: w.signer, Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res2.Rotated+res2.AlreadyRotated != 1 || res2.Failed != 0 {
		t.Fatalf("retry: rotated=%d already=%d failed=%d", res2.Rotated, res2.AlreadyRotated, res2.Failed)
	}
}
