package backup_test

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// ReadAttestationlessAuto must hand back the same manifest as the held and
// the streamed reader, whichever side of AutoStreamThreshold the object is
// on; `partial inspect` relies on it for deployments whose manifests are
// too large to hold.
func TestReadAttestationlessAuto_HeldAndStreamedAgree(t *testing.T) {
	priv, _, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	store, _ := storeAt(t)
	ctx := context.Background()
	m := validManifest()
	m.Files = manyFiles(300)
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	held, err := store.ReadAttestationlessAuto(ctx, m.Deployment, m.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(held.Files) != 300 {
		t.Fatalf("held read: %d files in memory, want 300", len(held.Files))
	}

	defer func(n int64) { backup.AutoStreamThreshold = n }(backup.AutoStreamThreshold)
	backup.AutoStreamThreshold = 1
	streamed, err := store.ReadAttestationlessAuto(ctx, m.Deployment, m.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	if streamed.Files != nil {
		t.Fatalf("streamed read held %d files in memory", len(streamed.Files))
	}
	if streamed.NumFiles() != held.NumFiles() || streamed.TotalFileBytes() != held.TotalFileBytes() ||
		streamed.NumChunkRefs() != held.NumChunkRefs() {
		t.Fatalf("held and streamed disagree: files %d/%d bytes %d/%d refs %d/%d",
			held.NumFiles(), streamed.NumFiles(), held.TotalFileBytes(), streamed.TotalFileBytes(),
			held.NumChunkRefs(), streamed.NumChunkRefs())
	}
	n := 0
	if err := streamed.EachFile(ctx, func(*backup.FileEntry) error { n++; return nil }); err != nil || n != 300 {
		t.Fatalf("EachFile visited %d (err %v), want 300", n, err)
	}

	// A caller error after delivery began is final and comes back intact.
	boom := errors.New("boom")
	if err := streamed.EachFile(ctx, func(*backup.FileEntry) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("EachFile error = %v, want boom", err)
	}
}
