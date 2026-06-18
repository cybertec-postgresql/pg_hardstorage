package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// A cancelled context mid-verify MUST NOT produce false-positive
// mismatches. Bug-review pass 6 found this: pre-fix, every chunk we
// didn't get to (e.g. on Ctrl-C) returned ctx.Err from GetChunkBytes,
// got recorded as a mismatch, and verify.chunk_mismatch fired with
// ExitVerifyFailed (9) — falsely flagging the repo as corrupt.
//
// This test exercises verifyChunks directly with an already-cancelled
// context to assert that:
//  1. the returned err is the ctx error (so the caller routes it to
//     aborted.*, not verify.*)
//  2. stats.Mismatches stays empty — no chunk is incorrectly
//     labelled as a verification finding.
func TestVerifyChunks_CtxCancelledIsNotAMismatch(t *testing.T) {
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	cas := casdefault.New(sp)
	info, err := cas.PutChunk(context.Background(), []byte("real chunk that exists"))
	if err != nil {
		t.Fatal(err)
	}

	m := &backup.Manifest{
		Schema:     backup.Schema,
		BackupID:   "db1.test.20260428T0000Z",
		Deployment: "db1",
		Files: []backup.FileEntry{{
			Path: "f", Size: 22,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: 22}},
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	stats, err := verifyChunks(ctx, cas, m, 0)
	if err == nil {
		t.Fatal("expected ctx error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if len(stats.Mismatches) != 0 {
		t.Errorf("ctx cancellation must NOT populate mismatches; got %d", len(stats.Mismatches))
	}
}

// And the same shape for a deadline-exceeded ctx, which is the
// production path for `verify --timeout`-style usage.
func TestVerifyChunks_DeadlineExceededIsNotAMismatch(t *testing.T) {
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	cas := casdefault.New(sp)
	info, _ := cas.PutChunk(context.Background(), []byte("body"))

	m := &backup.Manifest{
		Files: []backup.FileEntry{{
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Len: 4}},
		}},
	}

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	stats, err := verifyChunks(ctx, cas, m, 0)
	if err == nil {
		t.Fatal("expected ctx error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
	if len(stats.Mismatches) != 0 {
		t.Errorf("deadline-exceeded must NOT populate mismatches; got %d", len(stats.Mismatches))
	}
}

// Sanity check: with a fresh ctx and a real chunk, verifyChunks
// returns nil error and one verified chunk. Just a guard that the
// cancellation paths above don't accidentally trigger on the happy path.
func TestVerifyChunks_HappyPath(t *testing.T) {
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	cas := casdefault.New(sp)
	info, _ := cas.PutChunk(context.Background(), []byte("present"))

	m := &backup.Manifest{
		Files: []backup.FileEntry{{
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Len: 7}},
		}},
	}

	stats, err := verifyChunks(context.Background(), cas, m, 0)
	if err != nil {
		t.Fatalf("happy-path err: %v", err)
	}
	if stats.ChunksVerified != 1 || len(stats.Mismatches) != 0 {
		t.Errorf("verified=%d, mismatches=%d, want 1/0",
			stats.ChunksVerified, len(stats.Mismatches))
	}
}

// verifyChunks no longer re-hashes a chunk GetChunkBytes already
// content-address-verified (CPU-pathology audit #5 — that second SHA
// pass was an always-false check that doubled hashing CPU). This pins
// that corruption detection is UNCHANGED: a chunk whose stored bytes no
// longer hash to its key is reported as a mismatch, because
// GetChunkBytes errors with ErrChecksumMismatch and verifyChunks records
// that as a finding. Corruption is simulated by overwriting one chunk's
// object with another valid chunk's bytes — it decodes cleanly but
// hashes to the wrong key, exercising the SHA check inside GetChunkBytes
// that verify now relies on solely.
func TestVerifyChunks_DetectsCorruptedChunk(t *testing.T) {
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	cas := casdefault.New(sp)
	good, err := cas.PutChunk(context.Background(), []byte("the chunk we will reference"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := cas.PutChunk(context.Background(), []byte("a different chunk's bytes"))
	if err != nil {
		t.Fatal(err)
	}

	// Overwrite good's stored object with other's bytes: it still
	// decodes, but the plaintext now hashes to `other`, not `good`.
	rc, err := sp.Get(context.Background(), repo.ChunkKey(other.Hash))
	if err != nil {
		t.Fatal(err)
	}
	otherBytes, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Put(context.Background(), repo.ChunkKey(good.Hash),
		bytes.NewReader(otherBytes), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	m := &backup.Manifest{
		Files: []backup.FileEntry{{
			Chunks: []backup.ChunkRef{{Hash: good.Hash, Len: 27}},
		}},
	}

	stats, err := verifyChunks(context.Background(), cas, m, 0)
	if err != nil {
		t.Fatalf("verifyChunks err: %v", err)
	}
	if stats.ChunksVerified != 0 {
		t.Errorf("corrupt chunk must not count as verified; got %d", stats.ChunksVerified)
	}
	if len(stats.Mismatches) != 1 || stats.Mismatches[0] != good.Hash {
		t.Errorf("expected exactly the corrupt chunk in mismatches; got %v", stats.Mismatches)
	}
}
