package backup_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func storeAt(t *testing.T) (*backup.ManifestStore, storage.StoragePlugin) {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return backup.NewManifestStore(sp), sp
}

func manyFiles(n int) []backup.FileEntry {
	h := repo.Hash{5}
	out := make([]backup.FileEntry, n)
	for i := range out {
		out[i] = backup.FileEntry{
			Path: fmt.Sprintf("base/16384/<%d>&\"x\"", i), Size: 30, Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: h, Offset: 0, Len: 10}, {Hash: h, Offset: 10, Len: 20}},
		}
	}
	return out
}

func getRaw(t *testing.T, sp storage.StoragePlugin, key string) []byte {
	t.Helper()
	rc, err := sp.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return readAll(t, rc)
}

// CommitSpooled must publish byte-for-byte what Commit publishes (Ed25519
// is deterministic, so identical input means an identical signature).
func TestCommitSpooled_IdenticalToCommit(t *testing.T) {
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	ctx := context.Background()

	for _, n := range []int{1, 7, 2500} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			full := validManifest()
			full.Files = manyFiles(n)
			full.Dirs = []backup.DirEntry{{Path: "pg_wal", Mode: 0o700}}
			full.WALRequired = []string{"000000010000000000000003"}

			storeA, spA := storeAt(t)
			if err := storeA.Commit(ctx, full, signer, backup.CommitOptions{}); err != nil {
				t.Fatalf("Commit: %v", err)
			}

			header := validManifest()
			header.Files = nil
			header.Dirs = full.Dirs
			header.WALRequired = full.WALRequired
			header.StartedAt, header.StoppedAt = full.StartedAt, full.StoppedAt
			spool, err := backup.NewManifestSpool(t.TempDir(), 100)
			if err != nil {
				t.Fatal(err)
			}
			defer spool.Close()
			for i := range full.Files {
				if err := spool.Add(&full.Files[i]); err != nil {
					t.Fatal(err)
				}
			}
			storeB, spB := storeAt(t)
			if err := storeB.CommitSpooled(ctx, header, spool, signer, backup.CommitOptions{}); err != nil {
				t.Fatalf("CommitSpooled: %v", err)
			}

			for _, key := range []string{backup.PrimaryPath(full.Deployment, full.BackupID), backup.ReplicaPath(full.BackupID)} {
				a, b := getRaw(t, spA, key), getRaw(t, spB, key)
				if !bytes.Equal(a, b) {
					t.Fatalf("%s differs (len %d vs %d)", key, len(a), len(b))
				}
			}
			// And it reads back, held and streamed.
			if _, err := storeB.Read(ctx, full.Deployment, full.BackupID, verifier); err != nil {
				t.Fatalf("Read: %v", err)
			}
			sm, err := storeB.ReadStreaming(ctx, full.Deployment, full.BackupID, verifier)
			if err != nil || sm.NumFiles() != n {
				t.Fatalf("ReadStreaming: %v files=%d", err, sm.NumFiles())
			}
			if err := sm.Validate(); err != nil {
				t.Fatalf("streamed Validate: %v", err)
			}
			if spool.NumFiles() != int64(n) || spool.NumChunkRefs() != int64(2*n) || spool.TotalFileBytes() != int64(30*n) {
				t.Errorf("spool totals: %d files %d refs %d bytes", spool.NumFiles(), spool.NumChunkRefs(), spool.TotalFileBytes())
			}
		})
	}
}

func TestManifestSpool_RejectsBadEntriesAndRefusesPresigned(t *testing.T) {
	spool, err := backup.NewManifestSpool(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	good := manyFiles(1)[0]
	if err := spool.Add(&good); err != nil {
		t.Fatal(err)
	}
	dup := good
	if err := spool.Add(&dup); err == nil {
		t.Error("duplicate path accepted")
	}
	bad := backup.FileEntry{Path: "../escape", Size: 0}
	if err := spool.Add(&bad); err == nil {
		t.Error("escaping path accepted")
	}
	sum := backup.FileEntry{Path: "x", Size: 99, Chunks: []backup.ChunkRef{{Hash: repo.Hash{1}, Len: 1}}}
	if err := spool.Add(&sum); err == nil {
		t.Error("chunk-sum mismatch accepted")
	}

	store, _ := storeAt(t)
	priv, _, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	h := validManifest()
	h.Files = nil
	h.Attestation = &backup.Attestation{Scheme: backup.SchemeEd25519}
	if err := store.CommitSpooled(context.Background(), h, spool, signer, backup.CommitOptions{}); err == nil {
		t.Error("pre-signed manifest accepted")
	}
	h.Attestation = nil
	h.Files = manyFiles(1)
	if err := store.CommitSpooled(context.Background(), h, spool, signer, backup.CommitOptions{}); err == nil {
		t.Error("manifest with held Files accepted")
	}
	// Dir colliding with a spooled file is refused at commit.
	h.Files = nil
	h.Dirs = []backup.DirEntry{{Path: good.Path}}
	if err := store.CommitSpooled(context.Background(), h, spool, signer, backup.CommitOptions{}); err == nil {
		t.Error("dir/file collision accepted")
	}
}
