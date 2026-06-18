package backup_test

import (
	"bytes"
	"context"
	"crypto/rand"
	stdio "io"
	mathrand "math/rand"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/chunker"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestEndToEnd_ChunkerCASManifestSignCommitReadVerifyReconstitute is the
// headline integration test for Slice 5. It exercises every layer added
// since Slice 1:
//
//  1. Generate a random "file" body.
//  2. Chunk it via FastCDC.
//  3. Store every chunk in CAS, recording the chunk-ref list.
//  4. Build a Manifest pointing at those chunks.
//  5. Sign + commit the manifest atomically.
//  6. Re-open the repo, read the manifest, verify the signature.
//  7. Use the manifest's chunk-refs to fetch the chunks back.
//  8. Concatenate the chunks and confirm byte-equality with the input.
//
// Failure of any step aborts. Passing means the whole pipe is real.
func TestEndToEnd_ChunkerCASManifestSignCommitReadVerifyReconstitute(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	// --- step 1: random "file" body ---
	r := mathrand.New(mathrand.NewSource(0xBEEF))
	body := make([]byte, 2*1024*1024) // 2 MiB
	if _, err := stdio.ReadFull(r, body); err != nil {
		t.Fatal(err)
	}

	// --- shared storage plugin for the whole test ---
	sp := &fs.Plugin{}
	if err := sp.Open(ctx, storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	// --- step 2 + 3: chunk + store, building chunk refs ---
	cas := repo.NewCAS(sp)
	var chunkRefs []backup.ChunkRef
	var totalSize int64
	for ch, err := range chunker.New().Iter(bytes.NewReader(body)) {
		if err != nil {
			t.Fatalf("chunker: %v", err)
		}
		// Copy because the chunker reuses its buffer.
		buf := make([]byte, len(ch.Data))
		copy(buf, ch.Data)
		info, err := cas.PutChunk(ctx, buf)
		if err != nil {
			t.Fatalf("cas put: %v", err)
		}
		chunkRefs = append(chunkRefs, backup.ChunkRef{
			Hash:   info.Hash,
			Offset: ch.Offset,
			Len:    info.Size,
		})
		totalSize += info.Size
	}
	if totalSize != int64(len(body)) {
		t.Fatalf("chunk-total %d != input %d", totalSize, len(body))
	}

	// --- step 4: build manifest ---
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "db1.full.20260428T1300Z",
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        170,
		SystemIdentifier: "7388123456789012345",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        time.Date(2026, 4, 28, 13, 0, 0, 0, time.UTC),
		StoppedAt:        time.Date(2026, 4, 28, 13, 8, 23, 0, time.UTC),
		Compression:      "none",
		BackupLabel: "START WAL LOCATION: 0/3000028 (file 000000010000000000000003)\n" +
			"CHECKPOINT LOCATION: 0/3000028\n" +
			"BACKUP METHOD: streamed\n" +
			"BACKUP FROM: primary\n" +
			"START TIME: 2026-04-28 13:00:00 UTC\n" +
			"LABEL: pg_hardstorage_db1\n",
		Tablespaces: []backup.Tablespace{
			{OID: 1663, Location: "pg_default"},
		},
		Files: []backup.FileEntry{
			{
				Path:   "base/16384/2619",
				Size:   int64(len(body)),
				Mode:   0o644,
				Chunks: chunkRefs,
			},
		},
	}

	// --- step 5: sign + commit ---
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	store := backup.NewManifestStore(sp)
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// --- step 6: re-open through a fresh ManifestStore on the same
	//   repo, read the manifest, verify the signature. The freshness
	//   matters: we want to prove the on-disk bytes are self-sufficient,
	//   not that we got lucky with in-memory state. ---
	sp2 := &fs.Plugin{}
	if err := sp2.Open(ctx, storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	defer sp2.Close()
	store2 := backup.NewManifestStore(sp2)
	loaded, err := store2.Read(ctx, m.Deployment, m.BackupID, verifier)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if loaded.BackupID != m.BackupID {
		t.Errorf("BackupID round-trip: got %q, want %q", loaded.BackupID, m.BackupID)
	}
	if len(loaded.Files) != 1 || len(loaded.Files[0].Chunks) != len(chunkRefs) {
		t.Fatalf("manifest shape changed across round-trip: %+v", loaded)
	}

	// --- step 7 + 8: reconstitute using only the loaded manifest ---
	cas2 := repo.NewCAS(sp2)
	var rebuilt bytes.Buffer
	for _, ref := range loaded.Files[0].Chunks {
		chunk, err := cas2.GetChunkBytes(ctx, ref.Hash)
		if err != nil {
			t.Fatalf("get chunk %s: %v", ref.Hash, err)
		}
		if int64(len(chunk)) != ref.Len {
			t.Errorf("chunk %s len %d != ref %d", ref.Hash, len(chunk), ref.Len)
		}
		rebuilt.Write(chunk)
	}
	if !bytes.Equal(rebuilt.Bytes(), body) {
		t.Error("reconstituted bytes do not match original input")
	}
}

// TestEndToEnd_TamperedChunkBlocksReconstitution proves the read-time
// SHA-256 verification in CAS.GetChunkBytes catches on-disk corruption,
// even when the manifest itself verifies correctly. This is the "no
// silent bit-rot" guarantee the user-facing docs make.
func TestEndToEnd_TamperedChunkBlocksReconstitution(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	sp := &fs.Plugin{}
	if err := sp.Open(ctx, storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	cas := repo.NewCAS(sp)

	body := []byte("the bytes that will be on disk")
	info, err := cas.PutChunk(ctx, body)
	if err != nil {
		t.Fatal(err)
	}

	// Tamper: overwrite the on-disk chunk with different bytes via the
	// raw plugin (bypassing CAS so the IfNotExists guard doesn't fire).
	tampered := []byte("DIFFERENT bytes, same key — bit rot!")
	_, err = sp.Put(ctx, repo.ChunkKey(info.Hash), bytes.NewReader(tampered), storage.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// GetChunkBytes must detect the mismatch.
	if _, err := cas.GetChunkBytes(ctx, info.Hash); err == nil {
		t.Error("GetChunkBytes must refuse a tampered chunk")
	}
}
