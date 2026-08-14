package repo_test

// gc_orphan_agefloor_fuzz_test.go — the SECOND half of gc's reap decision,
// fuzzed. gc reaps a chunk IFF it is (a) unreferenced AND (b) old enough.
// gc_grace_refset_fuzz_test.go fuzzes half (a) — what CollectReferences
// considers referenced. This fuzzes half (b): FindOrphansWithOptions' age
// floor, the guard for the dedup-vs-GC race (#13).
//
// A backup writes its chunks (crash-durable via Barrier) BEFORE it commits
// the manifest that references them. Between those two steps the chunks are
// unreferenced-but-LIVE. A concurrent `repo gc --apply` whose reference
// snapshot predates that commit must NOT reap them — the age floor keeps any
// chunk younger than MinAge (or whose ModTime the backend hides) regardless
// of reference state. The invariant:
//
//   FindOrphansWithOptions returns a chunk  IFF  it is unreferenced AND its
//   ModTime is known AND at least MinAge old.
//
// A false POSITIVE (a young/unknown-age chunk returned) is the data-loss
// case: gc deletes a chunk an in-flight backup is about to reference,
// leaving the committed manifest pointing at deleted data. A false negative
// (an old orphan withheld) is only a reclamation leak. Both fail this fuzz
// because the expected set uses the SAME predicate the code uses
// (ModTime.IsZero() || ModTime.After(now-MinAge) ⇒ keep).
//
// FindOrphans reads only List keys + ModTime (never chunk content), so
// chunk objects are written as 1-byte placeholders and ages are driven
// through the same List-wrapping agedPlugin the refset fuzz uses.

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func FuzzFindOrphansNeverReapsYoungOrReferenced(f *testing.F) {
	f.Add([]byte{9, 0, 1, 2, 130, 0, 66, 1, 200, 2, 3, 0, 7, 1, 4})
	f.Add([]byte{4, 66, 130, 0, 200, 1, 2})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) < 2 {
			return
		}
		ctx := context.Background()
		sp := &fs.Plugin{}
		if err := sp.Open(ctx, storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
			t.Fatalf("open fs: %v", err)
		}
		t.Cleanup(func() { _ = sp.Close() })

		base := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
		minAge := time.Hour

		refs := repo.NewRefSet()
		ages := map[string]time.Time{}
		expected := map[string]struct{}{} // hex hashes that SHOULD be flagged orphan

		at := func(i int) byte {
			if i < 0 {
				i = -i
			}
			return raw[i%len(raw)]
		}

		n := int(raw[0]%24) + 1
		for i := 0; i < n; i++ {
			hex := fmt.Sprintf("%064x", i+1)
			h, err := repo.ParseHash(hex)
			if err != nil {
				t.Fatalf("ParseHash(%s): %v", hex, err)
			}
			key := repo.ChunkKey(h)
			if _, err := sp.Put(ctx, key, bytes.NewReader([]byte{0}), storage.PutOptions{ContentLength: 1}); err != nil {
				t.Fatalf("put chunk %s: %v", key, err)
			}

			b := at(i + 1)
			referenced := b&1 == 1
			if referenced {
				refs.Add(h)
			}
			// Age category from two bits: 0=old (past floor), 1=young
			// (within floor), 2=zero-mtime (backend hides it), 3=old.
			old := false
			switch (b >> 1) & 3 {
			case 0, 3:
				ages[key] = base.Add(-2 * minAge) // Before cutoff → old
				old = true
			case 1:
				ages[key] = base.Add(-minAge / 2) // After cutoff → young
			case 2:
				ages[key] = time.Time{} // zero → conservatively kept
			}
			if !referenced && old {
				expected[hex] = struct{}{}
			}
		}

		wrapped := &agedPlugin{StoragePlugin: sp, ages: ages}
		orphans, err := repo.FindOrphansWithOptions(ctx, wrapped, refs, repo.FindOrphansOptions{
			MinAge: minAge,
			Now:    base,
		})
		if err != nil {
			t.Fatalf("FindOrphansWithOptions: %v", err)
		}

		got := map[string]struct{}{}
		for _, h := range orphans {
			got[h.String()] = struct{}{}
		}

		// IFF: returned exactly the unreferenced-AND-old set. A young or
		// referenced chunk appearing here is the data-loss case.
		for i := 0; i < n; i++ {
			hex := fmt.Sprintf("%064x", i+1)
			_, want := expected[hex]
			_, have := got[hex]
			switch {
			case want && !have:
				t.Fatalf("chunk %s is unreferenced and past the age floor but was NOT flagged — reclamation leak", hex)
			case have && !want:
				t.Fatalf("chunk %s was flagged an orphan but is either REFERENCED or younger than the "+
					"age floor — gc would reap a live/in-flight chunk (DATA LOSS, the dedup-vs-GC race)", hex)
			}
		}
		if len(got) != len(expected) {
			t.Fatalf("orphan count %d != expected %d — a chunk outside the pool leaked in", len(got), len(expected))
		}
	})
}

// TestFindOrphans_AgeFloorKeepsYoungUnreferencedChunk pins the precondition
// the fuzz trusts, both directions: an unreferenced YOUNG chunk is withheld
// while the floor is active (so an in-flight backup's chunk survives a
// concurrent sweep) and IS flagged once the floor is disabled — proving the
// floor is what protects it, so the fuzz can't pass vacuously.
func TestFindOrphans_AgeFloorKeepsYoungUnreferencedChunk(t *testing.T) {
	ctx := context.Background()
	sp := &fs.Plugin{}
	if err := sp.Open(ctx, storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	base := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	minAge := time.Hour
	young, err := repo.ParseHash(fmt.Sprintf("%064x", 1))
	if err != nil {
		t.Fatal(err)
	}
	key := repo.ChunkKey(young)
	if _, err := sp.Put(ctx, key, bytes.NewReader([]byte{0}), storage.PutOptions{ContentLength: 1}); err != nil {
		t.Fatalf("put: %v", err)
	}
	ages := map[string]time.Time{key: base.Add(-minAge / 2)} // young
	wrapped := &agedPlugin{StoragePlugin: sp, ages: ages}
	refs := repo.NewRefSet() // nothing referenced

	// Floor ACTIVE: the young unreferenced chunk must be withheld.
	on, err := repo.FindOrphansWithOptions(ctx, wrapped, refs, repo.FindOrphansOptions{MinAge: minAge, Now: base})
	if err != nil {
		t.Fatalf("floor on: %v", err)
	}
	if len(on) != 0 {
		t.Fatalf("age floor active but a YOUNG unreferenced chunk was flagged (%v) — gc would reap an "+
			"in-flight backup's chunk (DATA LOSS)", on)
	}

	// Floor DISABLED (negative): now it IS an orphan — proves the floor is
	// the thing protecting it (so the fuzz's young-safety check has teeth).
	off, err := repo.FindOrphansWithOptions(ctx, wrapped, refs, repo.FindOrphansOptions{MinAge: -1, Now: base})
	if err != nil {
		t.Fatalf("floor off: %v", err)
	}
	if len(off) != 1 || off[0] != young {
		t.Fatalf("floor disabled: want [%s], got %v — the MinAge knob has no effect (fuzz would be vacuous)", young, off)
	}
}
