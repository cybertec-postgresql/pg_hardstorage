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
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// fanOutStorage wraps a StoragePlugin and tallies concurrent Stat
// calls, so a test can PROVE the commit gate verifies adopted chunks
// in parallel rather than serially.
type fanOutStorage struct {
	storage.StoragePlugin
	inflight atomic.Int64
	max      atomic.Int64

	// rendezvous, when non-nil, makes concurrency a FORCED question
	// instead of an observed one: each Stat parks until a second Stat
	// is in flight (or the timeout says none is coming). Merely
	// observing max-in-flight was a timing assertion — on a loaded
	// box (soak 24's stress phase, STRESS_COUNT repetition) every
	// local-fs Stat completed before the scheduler started the next
	// goroutine, the gate looked serial, and the test failed against
	// correct code. With the rendezvous a serial implementation
	// CANNOT pass (its single Stat waits alone until the timeout) and
	// a concurrent one cannot fail (two workers park together and
	// release each other), whatever the scheduler does.
	rendezvous      chan struct{}
	rendezvousTried atomic.Int64
}

func (f *fanOutStorage) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if cur := f.inflight.Add(1); cur > f.max.Load() {
		f.max.Store(cur)
	}
	defer f.inflight.Add(-1)
	// Only the first few Stats rendezvous, and none once overlap is
	// already proven: a serial gate then fails in ~4 timeouts instead
	// of one per chunk, and a concurrent gate pays the cost once.
	if f.rendezvous != nil && f.max.Load() < 2 && f.rendezvousTried.Add(1) <= 4 {
		select {
		case f.rendezvous <- struct{}{}: // a partner was parked — both proceed
		case <-f.rendezvous: // we un-park a partner
		case <-time.After(5 * time.Second):
			// No second Stat ever arrived: serial. Fall through — the
			// max-in-flight assertion reports it as the failure.
		}
	}
	return f.StoragePlugin.Stat(ctx, key)
}

// TestVerifyAdoptedChunks_StatsFanOut pins perf audit #4: the gate
// must not Stat adopted chunks one round-trip at a time — a
// heavily-deduped incremental carries hundreds of thousands of them,
// which is hours of serial Stats on object storage. With 128 adopted
// chunks the parallel gate observes >1 in flight and stays under the
// pool bound; the serial version observes exactly 1.
func TestVerifyAdoptedChunks_StatsFanOut(t *testing.T) {
	sp := &fanOutStorage{StoragePlugin: adoptTestRepo(t)}
	ctx := context.Background()

	const n = 128
	hints := make(map[repo.Hash]struct{}, n)
	bodies := make([][]byte, n)
	for i := range bodies {
		bodies[i] = []byte(fmt.Sprintf("fan-out chunk body %03d", i))
		hints[repo.HashOf(bodies[i])] = struct{}{}
	}
	// Seed the objects (an anonymous CAS does the writing).
	for _, b := range bodies {
		if _, err := casdefault.New(sp).PutChunk(ctx, b); err != nil {
			t.Fatalf("seed PutChunk: %v", err)
		}
	}

	// The gate's CAS adopts every seeded chunk without writing it.
	cas := casdefault.New(sp, casdefault.WithDedupHints(hints))
	for _, b := range bodies {
		info, err := cas.PutChunk(ctx, b)
		if err != nil {
			t.Fatalf("adopting PutChunk: %v", err)
		}
		if !info.Deduped {
			t.Fatal("fixture broken: chunk was not adopted — the test would measure nothing")
		}
	}
	if got := len(cas.AdoptedHashes()); got != n {
		t.Fatalf("AdoptedHashes = %d, want %d — fixture did not adopt", got, n)
	}

	sp.rendezvous = make(chan struct{})
	if err := verifyAdoptedChunks(ctx, sp, cas); err != nil {
		t.Fatalf("gate refused a healthy backup: %v", err)
	}
	sp.rendezvous = nil
	if got := sp.max.Load(); got < 2 {
		t.Errorf("max in-flight Stats = %d, want >1: the gate verified %d chunks serially", got, n)
	}
	if got := sp.max.Load(); got > statVerifyConcurrency {
		t.Errorf("max in-flight Stats = %d, want <= %d: the fan-out is not bounded", got, statVerifyConcurrency)
	}
}

// TestVerifyAdoptedChunks_CancelledContextStops: a cancelled commit
// context must stop the gate promptly with the context error, not
// finish the whole fan-out on a dead context.
func TestVerifyAdoptedChunks_CancelledContextStops(t *testing.T) {
	sp := &fanOutStorage{StoragePlugin: adoptTestRepo(t)}
	body := []byte("cancel-me")
	h := repo.HashOf(body)
	if _, err := casdefault.New(sp).PutChunk(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	cas := casdefault.New(sp, casdefault.WithDedupHints(map[repo.Hash]struct{}{h: {}}))
	if _, err := cas.PutChunk(context.Background(), body); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := verifyAdoptedChunks(ctx, sp, cas)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("gate on a cancelled context = %v, want context.Canceled", err)
	}
}
