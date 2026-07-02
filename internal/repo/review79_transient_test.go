package repo_test

import (
	"context"
	"errors"
	"io"
	"iter"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// errTransient is a stand-in for a throttling / network / permission
// failure — anything that is NOT storage.ErrNotFound. These must never be
// interpreted as "the object is absent".
var errTransient = errors.New("transient backend failure (throttled)")

// statFaultSP wraps a StoragePlugin and forces Stat to return errTransient
// for keys matching failKey. Everything else delegates. Used to prove that
// a transient Stat error is NOT mistaken for "not found".
type statFaultSP struct {
	storage.StoragePlugin
	failKey func(key string) bool
}

func (s *statFaultSP) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if s.failKey != nil && s.failKey(key) {
		return storage.ObjectInfo{}, errTransient
	}
	return s.StoragePlugin.Stat(ctx, key)
}

func (s *statFaultSP) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	return s.StoragePlugin.List(ctx, prefix)
}

func (s *statFaultSP) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.StoragePlugin.Get(ctx, key)
}

// TestFindMissing_TransientErrorAborts is the regression guard for bug 33:
// FindMissing must count a chunk missing ONLY on storage.ErrNotFound. A
// transient Stat error (throttle/network) must abort and propagate — never
// be reported as a missing chunk (which would falsely flag the backup as
// damaged).
func TestFindMissing_TransientErrorAborts(t *testing.T) {
	sp, cas := newGCRepo(t)
	a, _ := cas.PutChunk(context.Background(), []byte("alpha"))
	b, _ := cas.PutChunk(context.Background(), []byte("bravo"))

	refs := repo.NewRefSet()
	refs.Add(a.Hash)
	refs.Add(b.Hash)

	// Fail Stat for the second chunk's key with a transient error.
	failKey := repo.ChunkKey(b.Hash)
	fsp := &statFaultSP{
		StoragePlugin: sp,
		failKey:       func(k string) bool { return k == failKey },
	}

	missing, err := repo.FindMissing(context.Background(), fsp, refs)
	if err == nil {
		t.Fatalf("expected transient Stat error to propagate, got nil (missing=%v)", missing)
	}
	if !errors.Is(err, errTransient) {
		t.Fatalf("expected wrapped errTransient, got %v", err)
	}
	// The healthy chunk must NOT be reported missing on a transient error.
	for _, h := range missing {
		if h == b.Hash {
			t.Errorf("transient error falsely reported chunk %s as missing", b.Hash)
		}
	}
}

// TestFindMissing_NotFoundStillReported confirms the fix did not break the
// legitimate path: a genuine ErrNotFound is still reported as missing.
func TestFindMissing_NotFoundStillReported(t *testing.T) {
	sp, cas := newGCRepo(t)
	a, _ := cas.PutChunk(context.Background(), []byte("present"))
	dead, _ := cas.PutChunk(context.Background(), []byte("deleted"))
	if err := sp.Delete(context.Background(), repo.ChunkKey(dead.Hash)); err != nil {
		t.Fatal(err)
	}

	refs := repo.NewRefSet()
	refs.Add(a.Hash)
	refs.Add(dead.Hash)

	missing, err := repo.FindMissing(context.Background(), sp, refs)
	if err != nil {
		t.Fatalf("FindMissing: %v", err)
	}
	if len(missing) != 1 || missing[0] != dead.Hash {
		t.Errorf("want 1 missing (%s); got %v", dead.Hash, missing)
	}
}

// TestVerifyReplicate_TombstonedSkipped is the regression guard for bug 34:
// VerifyReplicate must skip tombstoned (soft-deleted) backups exactly as
// Replicate does. A replica that correctly lacks a tombstoned backup's
// manifest/chunks is HEALTHY, not broken.
func TestVerifyReplicate_TombstonedSkipped(t *testing.T) {
	src, dst := twoRepos(t)

	// Live backup: replicated to dst.
	liveChunk := putChunk(t, src, []byte("live-data"))
	putChunk(t, dst, []byte("live-data"))
	putManifest(t, src, "db1", "db1.full.alive", []repo.Hash{liveChunk})
	putManifest(t, dst, "db1", "db1.full.alive", []repo.Hash{liveChunk})

	// Tombstoned backup: manifest + chunk exist at src but were
	// deliberately NEVER copied to dst (Replicate skips tombstoned).
	deadChunk := putChunk(t, src, []byte("dead-data"))
	putManifest(t, src, "db1", "db1.full.gravestone", []repo.Hash{deadChunk})
	putRaw(t, src, "manifests/db1/backups/db1.full.gravestone/manifest.json.tombstone",
		[]byte(`{"backup_id":"db1.full.gravestone"}`))

	res, err := repo.VerifyReplicate(context.Background(), src, dst, repo.ReplicateVerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyReplicate: %v", err)
	}
	if res.Verdict != repo.VerdictConsistent {
		t.Errorf("verdict = %v, want Consistent (tombstoned backup should be skipped); failures=%+v",
			res.Verdict, res.Failures)
	}
	if res.ManifestsTombstoned != 1 {
		t.Errorf("ManifestsTombstoned = %d, want 1", res.ManifestsTombstoned)
	}
	// The tombstoned manifest must not have been counted as considered or
	// missing.
	if res.ManifestsMissing != 0 {
		t.Errorf("ManifestsMissing = %d, want 0", res.ManifestsMissing)
	}
	if res.ManifestsConsidered != 1 {
		t.Errorf("ManifestsConsidered = %d, want 1 (live only)", res.ManifestsConsidered)
	}
}

// TestVerifyReplicate_TransientDstError is the regression guard for bug 35:
// verifyKey must classify only storage.ErrNotFound as "missing at replica".
// A transient dst.Stat error must abort the verify (propagate) rather than
// declaring the healthy replica broken.
func TestVerifyReplicate_TransientDstError(t *testing.T) {
	src, dst := twoRepos(t)

	chunk := putChunk(t, src, []byte("body"))
	putChunk(t, dst, []byte("body"))
	putManifest(t, src, "db1", "db1.full.x", []repo.Hash{chunk})
	putManifest(t, dst, "db1", "db1.full.x", []repo.Hash{chunk})

	manifestKey := "manifests/db1/backups/db1.full.x/manifest.json"
	faultyDst := &statFaultSP{
		StoragePlugin: dst,
		failKey:       func(k string) bool { return k == manifestKey },
	}

	res, err := repo.VerifyReplicate(context.Background(), src, faultyDst, repo.ReplicateVerifyOptions{})
	if err == nil {
		t.Fatalf("expected transient dst.Stat error to abort verify, got nil (verdict=%v)", res.Verdict)
	}
	if !errors.Is(err, errTransient) {
		t.Fatalf("expected wrapped errTransient, got %v", err)
	}
	// A transient error must NOT be recorded as a missing manifest / broken.
	if res.ManifestsMissing != 0 {
		t.Errorf("transient error falsely counted %d missing manifests", res.ManifestsMissing)
	}
	if res.Verdict == repo.VerdictBroken {
		t.Errorf("transient error must not yield VerdictBroken")
	}
}

// TestVerifyReplicate_NotFoundStillBroken confirms the fix preserves the
// legitimate path: a manifest genuinely absent at dst (ErrNotFound) is still
// classified as missing → broken.
func TestVerifyReplicate_NotFoundStillBroken(t *testing.T) {
	src, dst := twoRepos(t)
	chunk := putChunk(t, src, []byte("only-at-src"))
	putManifest(t, src, "db1", "db1.full.orphan", []repo.Hash{chunk})
	// dst intentionally has nothing.

	res, err := repo.VerifyReplicate(context.Background(), src, dst, repo.ReplicateVerifyOptions{})
	if err != nil {
		t.Fatalf("VerifyReplicate: %v", err)
	}
	if res.ManifestsMissing == 0 {
		t.Errorf("genuinely-absent manifest should be reported missing")
	}
	if res.Verdict != repo.VerdictBroken {
		t.Errorf("verdict = %v, want Broken", res.Verdict)
	}
}
