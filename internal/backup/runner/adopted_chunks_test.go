package runner

// adopted_chunks_test.go — the commit-time gate for the dedup-vs-GC
// race, tested at the storage layer where the race actually lives.
//
// The scenario: backup B1's tombstone expires; its chunks become
// orphans. A new backup starts, and one of its files contains bytes
// identical to an orphaned chunk — dedup Stats the chunk, finds it,
// adopts it without writing. `repo gc --apply` then sweeps the orphan.
// The backup commits a manifest referencing a chunk that no longer
// exists: born broken, reporting success.
//
// gc's own guards (live-lease refusal, reference re-collect, second
// lease scan) are all TIMING guards — each shrinks the window, none
// closes it, because the adopt is invisible to gc: it is a Stat, it
// touches no object, it refreshes no mtime. The only place the race
// can be closed outright is the writer's commit, which is what
// verifyAdoptedChunks does: whatever the interleaving, a manifest only
// commits if every adopted chunk was present AFTER the last of them
// was adopted.

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

func adoptTestRepo(t *testing.T) storage.StoragePlugin {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

// TestVerifyAdoptedChunks_SweptAdoptionRefusesCommit is the race,
// replayed deterministically.
func TestVerifyAdoptedChunks_SweptAdoptionRefusesCommit(t *testing.T) {
	sp := adoptTestRepo(t)
	orphan := []byte("orphaned-chunk-content-from-an-expired-backup")
	h := repo.HashOf(orphan)

	// The orphan is already in the repo (written by the long-dead B1).
	if _, err := casdefault.New(sp).PutChunk(context.Background(), orphan); err != nil {
		t.Fatal(err)
	}

	// The new backup's CAS, hinted the way loadDedupHints hints it,
	// adopts the chunk without writing.
	cas := casdefault.New(sp, casdefault.WithDedupHints(map[repo.Hash]struct{}{h: {}}))
	info, err := cas.PutChunk(context.Background(), orphan)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Deduped {
		t.Fatal("fixture broken: the chunk was not adopted, so this test would pass " +
			"without exercising the race at all")
	}

	// gc sweeps the orphan between adopt and commit.
	if err := sp.Delete(context.Background(), repo.ChunkKey(h)); err != nil {
		t.Fatal(err)
	}

	gateErr := verifyAdoptedChunks(context.Background(), sp, cas)
	if gateErr == nil {
		t.Fatal("the commit gate passed with an adopted chunk deleted.\n\n" +
			"The manifest would commit referencing a chunk that no longer exists — a " +
			"brand-new backup that is already unrestorable, reporting success. Nothing " +
			"later can catch this cheaply: verify trusts the manifest, and the operator " +
			"trusts verify.")
	}
	for _, want := range []string{h.String()[:16], "repo gc", "Re-run the backup"} {
		if !strings.Contains(gateErr.Error(), want) {
			t.Errorf("gate error does not mention %q:\n%v", want, gateErr)
		}
	}
}

// TestVerifyAdoptedChunks_IntactAdoptionCommits: the gate must pass
// when the adopted chunks are still there — this runs on every backup
// with any dedup hit, so a false positive fails healthy backups.
func TestVerifyAdoptedChunks_IntactAdoptionCommits(t *testing.T) {
	sp := adoptTestRepo(t)
	orphan := []byte("still-present-chunk")
	h := repo.HashOf(orphan)
	if _, err := casdefault.New(sp).PutChunk(context.Background(), orphan); err != nil {
		t.Fatal(err)
	}
	cas := casdefault.New(sp, casdefault.WithDedupHints(map[repo.Hash]struct{}{h: {}}))
	if _, err := cas.PutChunk(context.Background(), orphan); err != nil {
		t.Fatal(err)
	}
	if err := verifyAdoptedChunks(context.Background(), sp, cas); err != nil {
		t.Fatalf("gate refused a healthy backup: %v", err)
	}
}

// TestVerifyAdoptedChunks_WrittenChunksAreNotStatted: chunks this run
// WROTE are outside the gate — they are protected by gc's
// --min-chunk-age floor, and re-Statting the whole manifest would turn
// the gate into a full existence sweep on every backup.
//
// Proven by deletion: the written chunk is removed before the gate
// runs, and the gate must still pass, because a written-then-deleted
// chunk is the age floor's failure to catch, not an adoption.
func TestVerifyAdoptedChunks_WrittenChunksAreNotStatted(t *testing.T) {
	sp := adoptTestRepo(t)
	cas := casdefault.New(sp)
	body := []byte("written-by-this-run")
	if _, err := cas.PutChunk(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	if err := sp.Delete(context.Background(), repo.ChunkKey(repo.HashOf(body))); err != nil {
		t.Fatal(err)
	}
	if err := verifyAdoptedChunks(context.Background(), sp, cas); err != nil {
		t.Fatalf("the gate Statted a chunk this run WROTE: %v\n\n"+
			"Written chunks are the age floor's job. Gating on them makes every backup "+
			"pay a full existence sweep and re-fails on transient losses the floor "+
			"already prevents.", err)
	}
}

// TestAdoptedHashes_LostPutRaceIsAdopted: the second adoption path — an
// IfNotExists put lost to a concurrent writer — must be gated too. The
// other writer's chunk is young, so gc's floor protects it today; but
// the floor is operator-configurable to 0, and the gate is the layer
// that owes no assumptions to gc's flags.
func TestAdoptedHashes_LostPutRaceIsAdopted(t *testing.T) {
	sp := adoptTestRepo(t)
	body := []byte("raced-chunk")
	h := repo.HashOf(body)
	// Another writer got there first.
	if _, err := casdefault.New(sp).PutChunk(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	// This CAS has no hint and no seen entry: its put loses the race.
	cas := casdefault.New(sp)
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Deduped {
		t.Fatal("fixture broken: expected the put to be deduplicated against the existing chunk")
	}
	var found bool
	for _, got := range cas.AdoptedHashes() {
		if got == h {
			found = true
		}
	}
	if !found {
		t.Errorf("a lost-put-race adoption is not in AdoptedHashes; the commit gate " +
			"would not re-verify it")
	}
}

// TestAdoptedHashes_MemoryHitsDoNotDuplicate: a second put of the same
// adopted content is a seen-cache hit and must not grow the set — the
// gate Stats each adopted chunk once, not once per occurrence.
func TestAdoptedHashes_MemoryHitsDoNotDuplicate(t *testing.T) {
	sp := adoptTestRepo(t)
	body := []byte("repeated-content")
	h := repo.HashOf(body)
	if _, err := casdefault.New(sp).PutChunk(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	cas := casdefault.New(sp, casdefault.WithDedupHints(map[repo.Hash]struct{}{h: {}}))
	for i := 0; i < 5; i++ {
		if _, err := cas.PutChunk(context.Background(), bytes.Clone(body)); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(cas.AdoptedHashes()); got != 1 {
		t.Errorf("AdoptedHashes has %d entries for one adopted chunk put 5 times, want 1", got)
	}
}
