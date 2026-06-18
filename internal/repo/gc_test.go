package repo_test

import (
	"context"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

func newGCRepo(t *testing.T) (storage.StoragePlugin, *repo.CAS) {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp, casdefault.New(sp)
}

func TestGC_FindOrphans_AllChunksReferenced(t *testing.T) {
	sp, cas := newGCRepo(t)

	// Put some chunks AND record their hashes in a fake "manifest" body
	// we feed CollectReferences via a hand-rolled Put.
	bodies := [][]byte{
		[]byte("alpha bravo charlie"),
		[]byte("delta echo foxtrot"),
	}
	infos := make([]repo.ChunkInfo, 0, len(bodies))
	for _, b := range bodies {
		ci, err := cas.PutChunk(context.Background(), b)
		if err != nil {
			t.Fatal(err)
		}
		infos = append(infos, ci)
	}

	// Write a minimal valid backup manifest referencing both chunks.
	manifestJSON := `{"files":[{"chunks":[`
	for i, ci := range infos {
		if i > 0 {
			manifestJSON += ","
		}
		manifestJSON += `{"hash":"` + ci.Hash.String() + `"}`
	}
	manifestJSON += `]}]}`
	if _, err := sp.Put(context.Background(), "manifests/db1/backups/test/manifest.json",
		readerOf(manifestJSON), storage.PutOptions{ContentLength: int64(len(manifestJSON))}); err != nil {
		t.Fatal(err)
	}

	refs, err := repo.CollectReferences(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	if refs.Len() != 2 {
		t.Errorf("refs.Len() = %d, want 2", refs.Len())
	}
	orphans, err := repo.FindOrphans(context.Background(), sp, refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Errorf("expected 0 orphans; got %d", len(orphans))
	}
}

func TestGC_FindOrphans_DetectsUnreferencedChunks(t *testing.T) {
	sp, cas := newGCRepo(t)

	// Two chunks; only ONE referenced.
	referenced, _ := cas.PutChunk(context.Background(), []byte("kept"))
	orphan, _ := cas.PutChunk(context.Background(), []byte("orphaned"))

	manifestJSON := `{"files":[{"chunks":[{"hash":"` + referenced.Hash.String() + `"}]}]}`
	if _, err := sp.Put(context.Background(), "manifests/db1/backups/test/manifest.json",
		readerOf(manifestJSON), storage.PutOptions{ContentLength: int64(len(manifestJSON))}); err != nil {
		t.Fatalf("put manifest: %v", err)
	}

	refs, _ := repo.CollectReferences(context.Background(), sp)
	orphans, err := repo.FindOrphans(context.Background(), sp, refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 {
		t.Fatalf("expected 1 orphan; got %d", len(orphans))
	}
	if orphans[0] != orphan.Hash {
		t.Errorf("orphan hash = %s, want %s", orphans[0], orphan.Hash)
	}
}

// TestGC_FindOrphans_ChunkAgeFloor pins the GC-vs-in-flight-backup
// race defense: a backup writes its chunks (durable via cas.Barrier)
// BEFORE committing the manifest that references them, so a guarded
// FindOrphans must NOT reap a freshly-written unreferenced chunk. Once
// the chunk ages past the floor it becomes eligible; a negative MinAge
// (and the raw FindOrphans primitive) disables the floor entirely.
func TestGC_FindOrphans_ChunkAgeFloor(t *testing.T) {
	sp, cas := newGCRepo(t)
	ctx := context.Background()

	// One unreferenced chunk, just written. No manifest references it,
	// mimicking a backup that wrote chunks but hasn't committed yet.
	orphan, _ := cas.PutChunk(ctx, []byte("fresh-unreferenced"))
	refs := repo.NewRefSet()

	// Default guard (24h): the fresh chunk is within the floor → kept.
	guarded, err := repo.FindOrphansWithOptions(ctx, sp, refs, repo.FindOrphansOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(guarded) != 0 {
		t.Fatalf("fresh unreferenced chunk must be protected by the age floor; got %d orphans", len(guarded))
	}

	// Viewed from 48h in the future the chunk is older than the 24h
	// floor → now eligible.
	aged, err := repo.FindOrphansWithOptions(ctx, sp, refs,
		repo.FindOrphansOptions{Now: time.Now().Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(aged) != 1 || aged[0] != orphan.Hash {
		t.Fatalf("aged-out unreferenced chunk should be reaped; got %v", aged)
	}

	// Disabled floor (MinAge<0) surfaces the fresh orphan immediately,
	// matching the raw FindOrphans primitive's no-floor behaviour.
	disabled, err := repo.FindOrphansWithOptions(ctx, sp, refs, repo.FindOrphansOptions{MinAge: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(disabled) != 1 {
		t.Fatalf("disabled floor should surface the fresh orphan; got %d", len(disabled))
	}
	raw, _ := repo.FindOrphans(ctx, sp, refs)
	if len(raw) != 1 {
		t.Fatalf("FindOrphans primitive must have no age floor; got %d", len(raw))
	}
}

// TestGC_FindStaleTempManifests pins the interrupted-commit cleanup:
// a `*.json.tmp.<rand>` staging file left by a commit whose process
// died between the tmp Put and the atomic rename must be flagged for
// removal (once past the age floor) while committed manifests are
// never touched.
func TestGC_FindStaleTempManifests(t *testing.T) {
	sp, _ := newGCRepo(t)
	ctx := context.Background()

	put := func(key, body string) {
		t.Helper()
		if _, err := sp.Put(ctx, key, readerOf(body),
			storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	// Committed manifests (must NEVER be flagged) ...
	put("manifests/db1/backups/b1/manifest.json", `{"files":[]}`)
	put("wal/db1/2/000000010000000000000003.json", `{"chunks":[]}`)
	// ... and the staging leftovers from interrupted commits.
	put("manifests/db1/backups/b1/manifest.json.tmp.deadbeefcafe", `{"files":[]}`)
	put("manifests/_replicas/b1.manifest.json.tmp.0badf00dface", `{"files":[]}`)
	put("wal/db1/2/000000010000000000000003.json.tmp.feedface1234", `{"chunks":[]}`)
	// timeline.Store's interrupted .history commit (randomized tmp).
	put("wal/db1/timelines/2.history.tmp.0123456789abcdef", "1\t0/3000028\tx\n")
	// A committed .history must NEVER be flagged.
	put("wal/db1/timelines/2.history", "1\t0/3000028\tx\n")

	// Default floor: the staging files are fresh → protected (a commit
	// could still be mid-flight between its tmp Put and rename).
	fresh, err := repo.FindStaleTempManifests(ctx, sp, repo.FindOrphansOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 0 {
		t.Fatalf("fresh staging files must be protected by the age floor; got %v", fresh)
	}

	// From 48h in the future the three staging files are stale; the
	// two committed manifests are never matched.
	stale, err := repo.FindStaleTempManifests(ctx, sp,
		repo.FindOrphansOptions{Now: time.Now().Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 4 {
		t.Fatalf("expected 4 stale staging files (3 .json.tmp + 1 .history.tmp); got %d: %v", len(stale), stale)
	}
	for _, k := range stale {
		if !strings.Contains(k, ".json.tmp.") && !strings.Contains(k, ".history.tmp.") {
			t.Errorf("non-staging key flagged for deletion: %s", k)
		}
		if k == "wal/db1/timelines/2.history" {
			t.Errorf("committed .history must not be flagged: %s", k)
		}
	}
}

func TestGC_TombstonedManifest_IsExcluded(t *testing.T) {
	// Chunks reachable only via a tombstoned manifest are GC
	// candidates ONCE the tombstone is older than the grace
	// window.  An audit added the grace; this test passes a
	// negative grace (disabled) so the tombstoned manifest is
	// immediately eligible — preserving the historical assertion
	// that "tombstone → chunks become orphans".
	sp, cas := newGCRepo(t)
	a, _ := cas.PutChunk(context.Background(), []byte("a"))
	b, _ := cas.PutChunk(context.Background(), []byte("b"))

	manifestJSON := `{"files":[{"chunks":[{"hash":"` + a.Hash.String() + `"},{"hash":"` + b.Hash.String() + `"}]}]}`
	if _, err := sp.Put(context.Background(), "manifests/db1/backups/test/manifest.json",
		readerOf(manifestJSON), storage.PutOptions{ContentLength: int64(len(manifestJSON))}); err != nil {
		t.Fatalf("put manifest: %v", err)
	}
	// Tombstone marker:
	tomb := `{"schema":"pg_hardstorage.tombstone.v1"}`
	if _, err := sp.Put(context.Background(),
		"manifests/db1/backups/test/manifest.json.tombstone",
		readerOf(tomb), storage.PutOptions{ContentLength: int64(len(tomb))}); err != nil {
		t.Fatalf("put tombstone: %v", err)
	}

	refs, _ := repo.CollectReferencesWithOptions(context.Background(), sp,
		repo.CollectReferencesOptions{TombstoneGrace: -1})
	if refs.Len() != 0 {
		t.Errorf("tombstoned manifest's refs should be excluded; got %d", refs.Len())
	}
	orphans, _ := repo.FindOrphans(context.Background(), sp, refs)
	if len(orphans) != 2 {
		t.Errorf("expected 2 orphans (both chunks); got %d", len(orphans))
	}
}

// TestGC_TombstonedManifest_GraceWindow_PreservesChunks: a
// tombstoned manifest within the grace window keeps its chunks
// in the reference set so an Undelete (within grace) recovers a
// fully-restorable backup. .
func TestGC_TombstonedManifest_GraceWindow_PreservesChunks(t *testing.T) {
	sp, cas := newGCRepo(t)
	a, _ := cas.PutChunk(context.Background(), []byte("a"))
	b, _ := cas.PutChunk(context.Background(), []byte("b"))

	manifestJSON := `{"files":[{"chunks":[{"hash":"` + a.Hash.String() + `"},{"hash":"` + b.Hash.String() + `"}]}]}`
	if _, err := sp.Put(context.Background(), "manifests/db1/backups/test/manifest.json",
		readerOf(manifestJSON), storage.PutOptions{ContentLength: int64(len(manifestJSON))}); err != nil {
		t.Fatalf("put manifest: %v", err)
	}
	tomb := `{"schema":"pg_hardstorage.tombstone.v1"}`
	if _, err := sp.Put(context.Background(),
		"manifests/db1/backups/test/manifest.json.tombstone",
		readerOf(tomb), storage.PutOptions{ContentLength: int64(len(tomb))}); err != nil {
		t.Fatalf("put tombstone: %v", err)
	}

	// Default grace (24h): tombstone is fresh, so refs survive.
	refs, _ := repo.CollectReferences(context.Background(), sp)
	if refs.Len() != 2 {
		t.Errorf("default grace should preserve fresh-tombstone refs; got %d, want 2", refs.Len())
	}
	orphans, _ := repo.FindOrphans(context.Background(), sp, refs)
	if len(orphans) != 0 {
		t.Errorf("expected 0 orphans within grace; got %d", len(orphans))
	}
}

// TestGC_TombstonedManifest_AfterGrace_ChunksOrphan: once the
// tombstone ages past the grace window, its manifest's chunks
// become GC candidates as expected.  Asserted by setting Now into
// the future.
func TestGC_TombstonedManifest_AfterGrace_ChunksOrphan(t *testing.T) {
	sp, cas := newGCRepo(t)
	a, _ := cas.PutChunk(context.Background(), []byte("a"))
	b, _ := cas.PutChunk(context.Background(), []byte("b"))

	manifestJSON := `{"files":[{"chunks":[{"hash":"` + a.Hash.String() + `"},{"hash":"` + b.Hash.String() + `"}]}]}`
	if _, err := sp.Put(context.Background(), "manifests/db1/backups/test/manifest.json",
		readerOf(manifestJSON), storage.PutOptions{ContentLength: int64(len(manifestJSON))}); err != nil {
		t.Fatalf("put manifest: %v", err)
	}
	tomb := `{"schema":"pg_hardstorage.tombstone.v1"}`
	if _, err := sp.Put(context.Background(),
		"manifests/db1/backups/test/manifest.json.tombstone",
		readerOf(tomb), storage.PutOptions{ContentLength: int64(len(tomb))}); err != nil {
		t.Fatalf("put tombstone: %v", err)
	}

	// Pretend "now" is a week in the future relative to the test
	// clock — the tombstone's mtime is then well outside the
	// 24h default grace.
	future := time.Now().Add(7 * 24 * time.Hour)
	refs, _ := repo.CollectReferencesWithOptions(context.Background(), sp,
		repo.CollectReferencesOptions{Now: future})
	if refs.Len() != 0 {
		t.Errorf("tombstone past grace → refs should be excluded; got %d", refs.Len())
	}
	orphans, _ := repo.FindOrphans(context.Background(), sp, refs)
	if len(orphans) != 2 {
		t.Errorf("expected 2 orphans past grace; got %d", len(orphans))
	}
}

func TestGC_FindMissing_DetectsDeletedChunks(t *testing.T) {
	sp, cas := newGCRepo(t)
	a, _ := cas.PutChunk(context.Background(), []byte("alive"))
	dead, _ := cas.PutChunk(context.Background(), []byte("about-to-die"))

	manifestJSON := `{"files":[{"chunks":[{"hash":"` + a.Hash.String() + `"},{"hash":"` + dead.Hash.String() + `"}]}]}`
	if _, err := sp.Put(context.Background(), "manifests/db1/backups/test/manifest.json",
		readerOf(manifestJSON), storage.PutOptions{ContentLength: int64(len(manifestJSON))}); err != nil {
		t.Fatalf("put manifest: %v", err)
	}

	// Delete one chunk out from under the manifest.
	if err := sp.Delete(context.Background(), repo.ChunkKey(dead.Hash)); err != nil {
		t.Fatal(err)
	}
	refs, _ := repo.CollectReferences(context.Background(), sp)
	missing, err := repo.FindMissing(context.Background(), sp, refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != dead.Hash {
		t.Errorf("expected 1 missing (%s); got %v", dead.Hash, missing)
	}
}

func TestGC_Scrub_VerifiesAllReferenced(t *testing.T) {
	sp, cas := newGCRepo(t)
	a, _ := cas.PutChunk(context.Background(), []byte("hello"))
	b, _ := cas.PutChunk(context.Background(), []byte("world"))

	manifestJSON := `{"files":[{"chunks":[{"hash":"` + a.Hash.String() + `"},{"hash":"` + b.Hash.String() + `"}]}]}`
	if _, err := sp.Put(context.Background(), "manifests/db1/backups/test/manifest.json",
		readerOf(manifestJSON), storage.PutOptions{ContentLength: int64(len(manifestJSON))}); err != nil {
		t.Fatalf("put manifest: %v", err)
	}

	refs, _ := repo.CollectReferences(context.Background(), sp)
	res, err := repo.Scrub(context.Background(), cas, refs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sampled != 2 || res.OK != 2 || len(res.Mismatches) != 0 {
		t.Errorf("scrub result %+v, want sampled=2 ok=2 mismatches=0", res)
	}
}

// External review pass: a cancelled ctx mid-Scrub used to surface
// every unfetched chunk's ctx.Err as a "mismatch" (same shape bug
// the verify command had — see Bug-review pass 6). The fix
// distinguishes ctx errors from real mismatches and returns the
// partial ScrubResult + the ctx error, so monitoring sees the
// abort as a clean cancellation rather than a verification finding.
func TestGC_Scrub_CtxCancelledIsNotAMismatch(t *testing.T) {
	sp, cas := newGCRepo(t)
	for _, body := range [][]byte{[]byte("a"), []byte("b"), []byte("c")} {
		if _, err := cas.PutChunk(context.Background(), body); err != nil {
			t.Fatal(err)
		}
	}
	manifestJSON := `{"files":[{"chunks":[`
	hashes := []repo.Hash{
		repo.HashOf([]byte("a")),
		repo.HashOf([]byte("b")),
		repo.HashOf([]byte("c")),
	}
	for i, h := range hashes {
		if i > 0 {
			manifestJSON += ","
		}
		manifestJSON += `{"hash":"` + h.String() + `"}`
	}
	manifestJSON += `]}]}`
	if _, err := sp.Put(context.Background(),
		"manifests/db1/backups/test/manifest.json",
		readerOf(manifestJSON), storage.PutOptions{ContentLength: int64(len(manifestJSON))}); err != nil {
		t.Fatalf("put manifest: %v", err)
	}

	refs, _ := repo.CollectReferences(context.Background(), sp)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	res, err := repo.Scrub(ctx, cas, refs, 0)
	if err == nil {
		t.Fatal("expected ctx error from Scrub on pre-cancelled ctx")
	}
	if len(res.Mismatches) != 0 {
		t.Errorf("ctx cancellation must NOT populate mismatches; got %d",
			len(res.Mismatches))
	}
}

// WAL segment manifests use a different shape than backup manifests
// (`chunks[]` vs `files[].chunks[]`). harvestWAL must pick this up
// when walking wal/ — a missed reference here means we'd
// orphan-collect chunks every WAL segment depends on for restore.
func TestGC_WALManifest_ContributesReferences(t *testing.T) {
	sp, cas := newGCRepo(t)
	a, _ := cas.PutChunk(context.Background(), []byte("wal-page-1"))
	b, _ := cas.PutChunk(context.Background(), []byte("wal-page-2"))

	walManifest := `{"chunks":[{"hash":"` + a.Hash.String() + `"},{"hash":"` + b.Hash.String() + `"}]}`
	// Realistic key under wal/<dep>/<TLI>/<seg>.json
	if _, err := sp.Put(context.Background(),
		"wal/db1/00000001/000000010000000000000003.json",
		readerOf(walManifest), storage.PutOptions{ContentLength: int64(len(walManifest))}); err != nil {
		t.Fatalf("put wal manifest: %v", err)
	}

	refs, err := repo.CollectReferences(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	if refs.Len() != 2 {
		t.Errorf("WAL refs not picked up: refs.Len() = %d, want 2", refs.Len())
	}
	orphans, _ := repo.FindOrphans(context.Background(), sp, refs)
	if len(orphans) != 0 {
		t.Errorf("WAL-referenced chunks reported as orphans: %v", orphans)
	}
}

// readerOf wraps strings.NewReader so the call sites read clearly.
// (The fs StoragePlugin's body reader expects io.EOF, not a custom
// sentinel — earlier I tried a hand-rolled reader and learned the
// hard way that interface contracts trump cleverness.)
func readerOf(s string) io.Reader { return strings.NewReader(s) }
