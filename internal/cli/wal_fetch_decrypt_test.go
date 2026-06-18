package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// TestWALFetch_DecryptsDedupedBackupChunk pins the fix for the
// cross-posture chunk collision: a plaintext WAL segment that deduped
// against an ENCRYPTED base-backup chunk (shared chunks/sha256/ namespace)
// must still be fetchable. The plain CAS fails the encrypted chunk with
// ErrUnknownAlgorithm; buildWALDecryptingCAS resolves the deployment's
// shared DEK from the keyring and the retry succeeds.
func TestWALFetch_DecryptsDedupedBackupChunk(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_ROOT", root)
	pth, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	kek, _, err := keystore.LoadOrGenerateKEK(pth.Keyring.Value)
	if err != nil {
		t.Fatalf("kek: %v", err)
	}

	repoRoot := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: repoRoot}}); err != nil {
		t.Fatalf("fs open: %v", err)
	}
	defer sp.Close()
	ctx := context.Background()

	// A DEK wrapped under the keyring KEK, recorded on a backup manifest.
	dek, err := encryption.GenerateDEK()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := encryption.Wrap(kek, dek)
	if err != nil {
		t.Fatal(err)
	}
	mani := &backup.Manifest{
		Schema: backup.Schema, BackupID: "db1.full.aaa", Deployment: "db1",
		Encryption: &backup.EncryptionInfo{
			Scheme: "aes-256-gcm", KEKRef: keystore.KEKRefLocal,
			WrappedDEK: base64.StdEncoding.EncodeToString(wrapped), EnvelopeVersion: 2,
		},
	}
	body, _ := json.Marshal(mani)
	if _, err := sp.Put(ctx, "manifests/db1/backups/db1.full.aaa/manifest.json",
		bytes.NewReader(body), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}

	// The backup encrypts an all-zeros chunk (an empty heap page region).
	enc, _ := aesgcm.New(dek[:])
	encCAS := casdefault.NewEncrypted(sp, enc)
	zeros := make([]byte, 256*1024)
	info, err := encCAS.PutChunk(ctx, zeros)
	if err != nil {
		t.Fatalf("encrypted PutChunk: %v", err)
	}

	// A WAL segment manifest references that same chunk (the .partial zero
	// tail deduped against the backup's encrypted chunk).
	seg := &walsink.SegmentManifest{
		Schema:        walsink.Schema,
		Timeline:      1,
		SegmentNumber: 1,
		SegmentName:   "000000010000000000000001",
		SegmentSize:   int64(len(zeros)),
		Chunks:        []walsink.ChunkRef{{Hash: info.Hash, Offset: 0, Len: int64(len(zeros))}},
	}

	target := filepath.Join(t.TempDir(), "seg.out")

	// The PLAIN WAL CAS fails the encrypted chunk — this is the trigger
	// condition the lazy retry keys on.
	plain := casdefault.New(sp)
	err = writeSegmentAtomically(ctx, plain, seg, target)
	if err == nil {
		t.Fatal("plain CAS unexpectedly read the encrypted chunk")
	}
	if !errors.Is(err, encryption.ErrUnknownAlgorithm) {
		t.Fatalf("plain CAS error = %v, want ErrUnknownAlgorithm (the retry trigger)", err)
	}

	// The decrypting CAS, built from the keyring, reassembles the segment.
	decCAS, ok := buildWALDecryptingCAS(ctx, sp, "db1")
	if !ok {
		t.Fatal("buildWALDecryptingCAS failed to resolve the shared DEK from the keyring")
	}
	if err := writeSegmentAtomically(ctx, decCAS, seg, target); err != nil {
		t.Fatalf("decrypting CAS failed to fetch the segment: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, zeros) {
		t.Errorf("fetched segment bytes != original (len got=%d want=%d)", len(got), len(zeros))
	}
}
