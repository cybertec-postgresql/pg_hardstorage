package backup_test

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// flakyPut fails the first Put of a manifest after consuming half the body
// (a connection dropped mid-upload), then behaves.
type flakyPut struct {
	storage.StoragePlugin
	failures int
}

func (f *flakyPut) Put(ctx context.Context, key string, r io.Reader, o storage.PutOptions) (storage.PutResult, error) {
	if f.failures > 0 && filepath.Base(key) != "" && o.ContentLength > 1000 {
		f.failures--
		_, _ = io.CopyN(io.Discard, r, o.ContentLength/2)
		return storage.PutResult{}, errors.New("injected: connection reset mid-upload")
	}
	return f.StoragePlugin.Put(ctx, key, r, o)
}

func spooled(t *testing.T, n int) (*backup.ManifestSpool, *backup.Manifest) {
	t.Helper()
	spool, err := backup.NewManifestSpool(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { spool.Close() })
	fs := manyFiles(n)
	for i := range fs {
		if err := spool.Add(&fs[i]); err != nil {
			t.Fatal(err)
		}
	}
	h := validManifest()
	h.Files = nil
	return spool, h
}

func TestCommitSpooled_FailedUploadLeavesNoManifestAndRetrySucceeds(t *testing.T) {
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	_, sp := storeAt(t)
	flaky := &flakyPut{StoragePlugin: sp, failures: 1}
	store := backup.NewManifestStore(flaky)
	ctx := context.Background()
	spool, h := spooled(t, 300)

	if err := store.CommitSpooled(ctx, h, spool, signer, backup.CommitOptions{}); err == nil {
		t.Fatal("commit succeeded despite a failed upload")
	}
	if _, err := sp.Stat(ctx, backup.PrimaryPath(h.Deployment, h.BackupID)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a failed commit left a primary manifest behind: %v", err)
	}
	if _, err := store.Read(ctx, h.Deployment, h.BackupID, verifier); err == nil {
		t.Fatal("a backup whose commit failed is readable")
	}
	// The spool survives the failure; the same entries commit on retry.
	h2 := validManifest()
	h2.Files = nil
	if err := store.CommitSpooled(ctx, h2, spool, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if m, err := store.ReadStreaming(ctx, h2.Deployment, h2.BackupID, verifier); err != nil || m.NumFiles() != 300 {
		t.Fatalf("retry result: %v files=%v", err, m)
	}
}

func TestManifestSpool_UnwritableSpoolDirFailsCleanly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if _, err := backup.NewManifestSpool(dir, 0); err == nil {
		t.Fatal("spool created in an unwritable directory")
	}
}

func TestCommitSpooled_CancelledContextLeavesNothing(t *testing.T) {
	priv, _, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	store, sp := storeAt(t)
	spool, h := spooled(t, 50)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.CommitSpooled(ctx, h, spool, signer, backup.CommitOptions{}); err == nil {
		t.Fatal("cancelled commit succeeded")
	}
	if _, err := sp.Stat(context.Background(), backup.PrimaryPath(h.Deployment, h.BackupID)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cancelled commit left a manifest: %v", err)
	}
}
