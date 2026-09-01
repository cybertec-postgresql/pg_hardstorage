package runner

// adopted_unchecked_test.go — a Stat that FAILS is not a Stat that
// passed.
//
// verifyAdoptedChunks re-Stats every chunk this backup deduplicated
// against rather than wrote, and refuses the commit if one is gone. Its
// own header and the call site both state the guarantee in absolute
// terms: "whatever the interleaving, a manifest only commits if every
// adopted chunk was present AFTER the last of them was adopted", and
// "Both are timing guards; this one is not."
//
// That was not true. A Stat failing for a reason OTHER than not-found —
// a 503 storm, a throttled bucket, exactly the conditions under which a
// backup and a gc run are most likely to be racing in the first place —
// hit a branch that did nothing at all. The chunk's presence went
// unconfirmed, the gate returned nil, and the manifest committed as if
// fully verified.
//
// The decision to PROCEED is sound and is kept: refusing the backup
// would trade a possible gap for a certain one. What is not sound is
// reporting a gate as passed when part of it never ran. The count is
// now returned so the run can say so.

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/faultinject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

var errStatStorm = errors.New("simulated 503 storm")

func TestVerifyAdoptedChunks_UnreachableBackendIsCountedNotPassed(t *testing.T) {
	inner := adoptTestRepo(t)
	fi := faultinject.New(inner)

	body := []byte("adopted-under-a-flaky-backend")
	h := repo.HashOf(body)
	if _, err := casdefault.New(inner).PutChunk(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	cas := casdefault.New(inner, casdefault.WithDedupHints(map[repo.Hash]struct{}{h: {}}))
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Deduped {
		t.Fatal("fixture broken: the chunk was not adopted, so the gate has nothing to check")
	}

	// The backend starts refusing chunk Stats with something that is
	// NOT not-found — the case the gate silently swallowed.
	fi.Activate([]faultinject.Rule{{
		Name:      "chunk-stat-storm",
		Ops:       faultinject.OpStat,
		KeyPrefix: "chunks/",
		Err:       errStatStorm,
	}}, faultinject.ActivateOptions{})
	defer fi.Deactivate()

	unchecked, gateErr := verifyAdoptedChunks(context.Background(), fi, cas)

	// Proceeding is correct: refusing would trade a possible gap for a
	// certain one.
	if gateErr != nil {
		t.Fatalf("gate refused the backup on a transient backend error: %v\n"+
			"failing here turns a POSSIBLE gap into a certain one — no new backup at all", gateErr)
	}
	// Reporting it as verified is not.
	if unchecked != 1 {
		t.Fatalf("unchecked = %d, want 1.\n\nThe gate returned a clean pass over a chunk whose "+
			"presence it never confirmed, so the manifest commits claiming a guarantee "+
			"(\"a manifest only commits over chunks that were present\") that this run did "+
			"not establish.", unchecked)
	}
}

// A not-found must stay a hard refusal — the new counter must not
// swallow the case the gate exists for.
func TestVerifyAdoptedChunks_MissingChunkStillRefuses(t *testing.T) {
	sp := adoptTestRepo(t)
	body := []byte("swept-between-adopt-and-commit")
	h := repo.HashOf(body)
	if _, err := casdefault.New(sp).PutChunk(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	cas := casdefault.New(sp, casdefault.WithDedupHints(map[repo.Hash]struct{}{h: {}}))
	if _, err := cas.PutChunk(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	if err := sp.Delete(context.Background(), repo.ChunkKey(h)); err != nil {
		t.Fatal(err)
	}
	unchecked, gateErr := verifyAdoptedChunks(context.Background(), sp, cas)
	if gateErr == nil {
		t.Fatal("a deleted adopted chunk no longer refuses the commit")
	}
	if unchecked != 0 {
		t.Errorf("unchecked = %d for a not-found; a missing chunk is a REFUSAL, not an "+
			"unverified one, and conflating them would let the counter absorb the "+
			"failure the gate exists for", unchecked)
	}
}

// The healthy path must report zero, or every deduping backup emits the
// warning and it stops carrying information.
func TestVerifyAdoptedChunks_HealthyRunReportsZeroUnchecked(t *testing.T) {
	sp := adoptTestRepo(t)
	body := []byte("present-throughout")
	h := repo.HashOf(body)
	if _, err := casdefault.New(sp).PutChunk(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	cas := casdefault.New(sp, casdefault.WithDedupHints(map[repo.Hash]struct{}{h: {}}))
	if _, err := cas.PutChunk(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	unchecked, err := verifyAdoptedChunks(context.Background(), sp, cas)
	if err != nil || unchecked != 0 {
		t.Fatalf("healthy run: (unchecked=%d, err=%v), want (0, nil)", unchecked, err)
	}
}

var _ storage.StoragePlugin = (*faultinject.Middleware)(nil)
