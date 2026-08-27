package repo_test

// replicate_adopted_test.go — the replica-side dedup-vs-GC race.
//
// Replicate adopts chunks already present at dst by Stat, exactly as
// the CAS adopts by stat-hint during a backup. The backup path
// re-verifies its adopted set at manifest-commit time
// (verifyAdoptedChunks) because an adopted chunk is an orphan with an
// OLD mtime until the manifest lands — precisely what a concurrent
// `repo gc --apply` sweeps. Replicate trusted its adoptions for the
// whole remainder of the copy, so a sweep on the REPLICA between
// adoption and manifest commit produced a committed manifest over a
// missing chunk: replication reports success, restore-from-replica
// fails. These tests drive repo.Replicate through that exact
// interleaving with a dst wrapper that sweeps a chunk at its adoption
// moment.

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// sweepOnAdoptPlugin lets the chunk's adoption Stat succeed and then
// immediately deletes the object — the tightest possible gc
// interleaving: adopted, then gone before anything else happens.
// Every other method delegates.
type sweepOnAdoptPlugin struct {
	storage.StoragePlugin
	key   string
	fired atomic.Bool
}

func (p *sweepOnAdoptPlugin) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	info, err := p.StoragePlugin.Stat(ctx, key)
	if err == nil && key == p.key && p.fired.CompareAndSwap(false, true) {
		if derr := p.StoragePlugin.Delete(ctx, key); derr != nil {
			return info, derr
		}
	}
	return info, err
}

// chunkStatCounter counts Stats under chunks/ so a test can assert
// the re-verify pass did NOT run.
type chunkStatCounter struct {
	storage.StoragePlugin
	chunkStats atomic.Int64
}

func (p *chunkStatCounter) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if strings.HasPrefix(key, "chunks/") {
		p.chunkStats.Add(1)
	}
	return p.StoragePlugin.Stat(ctx, key)
}

// seedDstChunk plants the same chunk body at dst so replicate adopts
// it instead of copying, and returns its storage key. Chunks are
// keyed by content, so the same body through the same (plaintext)
// CAS yields the key replicate will adopt — mirroring rvWorld's
// commitToBoth exactly.
func seedDstChunk(t *testing.T, w *rvWorld, body []byte) string {
	t.Helper()
	info, err := casdefault.New(w.dstSP).PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	return repo.ChunkKey(info.Hash)
}

func TestReplicate_AdoptedChunkSweptBeforeCommit_IsRecopied(t *testing.T) {
	w := setupRVWorld(t)
	body := []byte("adopted chunk body — swept by gc mid-replication")
	id := w.commitToBoth(t, "db1", 1, body, false /* no replicate yet */)
	key := seedDstChunk(t, w, body)

	dst := &sweepOnAdoptPlugin{StoragePlugin: w.dstSP, key: key}
	res, err := repo.Replicate(context.Background(), w.srcSP, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}

	if res.ChunksRecopied != 1 {
		t.Errorf("ChunksRecopied = %d, want 1 — the commit-time re-verify did not detect the sweep", res.ChunksRecopied)
	}
	if res.ManifestsFailed != 0 {
		t.Errorf("ManifestsFailed = %d, want 0 (the vanished chunk is re-copyable from src)", res.ManifestsFailed)
	}
	// The replica must be RESTORABLE: manifest present AND chunk present.
	if _, err := w.dstSP.Stat(context.Background(), key); err != nil {
		t.Errorf("chunk absent at replica after replicate reported success: %v", err)
	}
	if _, err := w.dstStore.Read(context.Background(), "db1", id, w.verifier); err != nil {
		t.Errorf("manifest unreadable at replica: %v", err)
	}
}

func TestReplicate_AdoptedChunkSweptAndSrcGone_ManifestWithheld(t *testing.T) {
	w := setupRVWorld(t)
	body := []byte("adopted chunk body — swept at dst, missing at src")
	id := w.commitToBoth(t, "db1", 1, body, false)
	key := seedDstChunk(t, w, body)
	// src loses the chunk too (e.g. it was already gc'd there after a
	// tombstone) — the re-copy has nowhere to pull from.
	if err := w.srcSP.Delete(context.Background(), key); err != nil {
		t.Fatal(err)
	}

	dst := &sweepOnAdoptPlugin{StoragePlugin: w.dstSP, key: key}
	res, err := repo.Replicate(context.Background(), w.srcSP, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}

	if res.ManifestsFailed != 1 {
		t.Errorf("ManifestsFailed = %d, want 1 — a manifest over a missing, un-recopyable chunk must be withheld", res.ManifestsFailed)
	}
	// The manifest must NOT be at the replica: committing it would be
	// a DR copy that lies about restorability.
	if _, err := w.dstStore.Read(context.Background(), "db1", id, w.verifier); err == nil {
		t.Errorf("manifest committed at replica over a chunk that exists nowhere")
	}
}

func TestReplicate_UpToDateReplica_NoReverifyCost(t *testing.T) {
	w := setupRVWorld(t)
	body := []byte("steady-state chunk")
	w.commitToBoth(t, "db1", 1, body, true /* replicate now */)

	// Re-run over the up-to-date replica. The manifest is already
	// committed there, which pins its chunks in gc's reference walk —
	// so the re-verify must short-circuit on the manifest Stat and
	// touch no chunk a second time.
	dst := &chunkStatCounter{StoragePlugin: w.dstSP}
	res, err := repo.Replicate(context.Background(), w.srcSP, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if res.ChunksRecopied != 0 {
		t.Errorf("ChunksRecopied = %d, want 0 on a steady-state re-run", res.ChunksRecopied)
	}
	if got := dst.chunkStats.Load(); got != 1 {
		t.Errorf("chunk Stats on a no-op re-run = %d, want 1 (adoption only; a second full "+
			"pass would double the cost of every nightly re-replication)", got)
	}
}
