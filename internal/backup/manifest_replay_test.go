package backup_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// A validly-signed manifest copied over a different backup's key (an
// older backup replayed as a newer one) must be refused by every read
// path: Read, StreamFiles and List.
func TestManifestStore_ReplayedManifestRefused(t *testing.T) {
	store, sp, signer, verifier := newStore(t)
	ctx := context.Background()
	a := sampleManifest()
	b := sampleManifest()
	b.BackupID = "db1.full.20260429T1200Z"
	for _, m := range []*backup.Manifest{a, b} {
		if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	rc, err := sp.Get(ctx, backup.PrimaryPath(a.Deployment, a.BackupID))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	for _, key := range []string{backup.PrimaryPath(b.Deployment, b.BackupID), backup.ReplicaPath(b.BackupID)} {
		if _, err := sp.Put(ctx, key, bytes.NewReader(body), storage.PutOptions{}); err != nil {
			// Overwrite may need a delete first on conditional-put backends.
			_ = sp.Delete(ctx, key)
			if _, err := sp.Put(ctx, key, bytes.NewReader(body), storage.PutOptions{}); err != nil {
				t.Fatalf("overwrite %s: %v", key, err)
			}
		}
	}
	if m, err := store.Read(ctx, b.Deployment, b.BackupID, verifier); err == nil {
		t.Fatalf("Read served %s as %s", m.BackupID, b.BackupID)
	}
	if _, err := store.StreamFiles(ctx, b.Deployment, b.BackupID, verifier, func(*backup.FileEntry) error { return nil }); err == nil {
		t.Fatal("StreamFiles served a replayed manifest")
	}
	sawErr := false
	for m, err := range store.List(ctx, b.Deployment, verifier) {
		if err != nil {
			sawErr = true
		} else if m.BackupID == a.BackupID && false {
			t.Fatal("unreachable")
		}
	}
	if !sawErr {
		t.Fatal("List did not flag the replayed manifest")
	}
	// The untouched backup still reads fine.
	if _, err := store.Read(ctx, a.Deployment, a.BackupID, verifier); err != nil {
		t.Fatalf("honest backup unreadable: %v", err)
	}
}
