package repo_test

// gc_grace_refset_fuzz_test.go — the tombstone-GRACE × chunk-SHARING data-
// loss invariant of gc's reference collection, fuzzed.
//
// gc reaps any chunk absent from CollectReferences' set. The tombstone
// grace exists so an Undelete fired BEFORE grace elapses recovers a fully
// restorable backup — which requires that a within-grace tombstoned
// backup's chunks stay in the reference set. Dedup makes this a set-algebra
// problem: a single chunk can be shared (same SHA) between a live backup, a
// young-tombstoned one, and a past-grace tombstoned one. The invariant:
//
//   a chunk is in the reference set  IFF  at least one LIVE or WITHIN-GRACE
//   tombstoned backup references it.
//
// The dangerous direction is a false ABSENCE — a chunk a live/young backup
// needs left out of the set, so `repo gc --apply` deletes it and renders
// that backup (or the undelete of that tombstone) unrestorable. The other
// direction (a past-grace-only chunk wrongly retained) is a reclamation
// leak, not data loss, but a grace-boundary bug in either direction fails
// this fuzz because it computes the expected set with the SAME predicate gc
// uses (ModTime.IsZero() || ModTime.After(now-grace) ⇒ young/live).
//
// CollectReferences only PARSES manifest JSON for chunk hashes, so manifests
// are written directly (no CAS population needed); tombstone ages are driven
// through a List-wrapping plugin that reports a controlled ModTime per key.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// agedPlugin reports a controlled ModTime for selected keys in List
// results, so a fuzz can place tombstone markers inside or outside the
// grace window deterministically. Every other method delegates.
type agedPlugin struct {
	storage.StoragePlugin
	ages map[string]time.Time
}

func (p *agedPlugin) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	inner := p.StoragePlugin.List(ctx, prefix)
	return func(yield func(storage.ObjectInfo, error) bool) {
		inner(func(info storage.ObjectInfo, err error) bool {
			if err == nil {
				if mt, ok := p.ages[info.Key]; ok {
					info.ModTime = mt
				}
			}
			return yield(info, err)
		})
	}
}

func backupManifestJSON(t *testing.T, chunkHexes []string) []byte {
	t.Helper()
	type ref struct {
		Hash string `json:"hash"`
	}
	type file struct {
		Chunks []ref `json:"chunks"`
	}
	var f file
	for _, h := range chunkHexes {
		f.Chunks = append(f.Chunks, ref{Hash: h})
	}
	body, err := json.Marshal(struct {
		Files []file `json:"files"`
	}{Files: []file{f}})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return body
}

// TestGCGrace_YoungTombstoneChunkRetainedNotOld pins the grace behaviour the
// fuzz above trusts, in both directions, deterministically: a young
// tombstone's UNIQUE chunk stays referenced (so Undelete-within-grace
// recovers a restorable backup) and a past-grace tombstone's unique chunk
// does NOT (so gc can eventually reclaim it). Without this, the fuzz could
// pass vacuously if CollectReferences ever stopped distinguishing the two.
func TestGCGrace_YoungTombstoneChunkRetainedNotOld(t *testing.T) {
	ctx := context.Background()
	sp := &fs.Plugin{}
	if err := sp.Open(ctx, storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	base := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	grace := time.Hour
	live := fmt.Sprintf("%064x", 1)  // referenced by a live backup
	young := fmt.Sprintf("%064x", 2) // referenced only by a YOUNG tombstone
	old := fmt.Sprintf("%064x", 3)   // referenced only by a PAST-GRACE tombstone

	put := func(key string, body []byte) {
		if _, err := sp.Put(ctx, key, bytes.NewReader(body), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	mkBackup := func(id, chunk string, tomb *time.Time, ages map[string]time.Time) {
		mkey := "manifests/db1/backups/" + id + "/manifest.json"
		put(mkey, backupManifestJSON(t, []string{chunk}))
		if tomb != nil {
			tkey := mkey + ".tombstone"
			put(tkey, []byte("{}"))
			ages[tkey] = *tomb
		}
	}

	ages := map[string]time.Time{}
	youngMT := base.Add(-grace / 2)
	oldMT := base.Add(-2 * grace)
	mkBackup("b000", live, nil, ages)
	mkBackup("b001", young, &youngMT, ages)
	mkBackup("b002", old, &oldMT, ages)
	wrapped := &agedPlugin{StoragePlugin: sp, ages: ages}

	has := func(refs *repo.RefSet, hex string) bool {
		h, err := repo.ParseHash(hex)
		if err != nil {
			t.Fatalf("ParseHash: %v", err)
		}
		return refs.Has(h)
	}

	// Grace ON: live + young retained, old reclaimable.
	refs, err := repo.CollectReferencesWithOptions(ctx, wrapped, repo.CollectReferencesOptions{TombstoneGrace: grace, Now: base})
	if err != nil {
		t.Fatalf("collect (grace on): %v", err)
	}
	if !has(refs, live) {
		t.Fatal("live chunk missing with grace on")
	}
	if !has(refs, young) {
		t.Fatal("within-grace tombstone's chunk was dropped — Undelete would recover a broken backup (DATA LOSS)")
	}
	if has(refs, old) {
		t.Fatal("past-grace tombstone's chunk retained — reclamation leak")
	}

	// Grace OFF (negative disables it): only the live chunk survives.
	refs2, err := repo.CollectReferencesWithOptions(ctx, wrapped, repo.CollectReferencesOptions{TombstoneGrace: -1, Now: base})
	if err != nil {
		t.Fatalf("collect (grace off): %v", err)
	}
	if !has(refs2, live) {
		t.Fatal("live chunk missing with grace off")
	}
	if has(refs2, young) {
		t.Fatal("grace disabled but young tombstone's chunk still retained — grace knob has no effect (fuzz would be vacuous)")
	}
}

func FuzzGCRefSetKeepsLiveAndYoungTombstonedChunks(f *testing.F) {
	f.Add([]byte{6, 33, 1, 5, 0, 2, 130, 3, 7, 200, 2, 9, 66, 1, 4, 0, 3, 8})
	f.Add([]byte{3, 0, 2, 1, 130, 1, 2, 66, 3})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) < 4 {
			return
		}
		ctx := context.Background()
		root := t.TempDir()
		sp := &fs.Plugin{}
		if err := sp.Open(ctx, storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
			t.Fatalf("open fs: %v", err)
		}
		t.Cleanup(func() { _ = sp.Close() })

		base := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
		grace := time.Hour
		graceCutoff := base.Add(-grace)

		const poolSize = 12
		hashOf := func(k int) string { return fmt.Sprintf("%064x", (k%poolSize)+1) }
		at := func(i int) byte {
			if i < 0 {
				i = -i
			}
			return raw[i%len(raw)]
		}

		ages := map[string]time.Time{}
		expected := map[string]struct{}{} // hashes a live/young backup references

		n := int(raw[0]%16) + 1
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("b%03d", i)

			// 1..3 chunks from the shared pool → dedup sharing across backups.
			nc := int(at(i*4+1)%3) + 1
			var chunks []string
			for j := 0; j < nc; j++ {
				chunks = append(chunks, hashOf(int(at(i*4+2+j))))
			}

			mkey := "manifests/db1/backups/" + id + "/manifest.json"
			body := backupManifestJSON(t, chunks)
			if _, err := sp.Put(ctx, mkey, bytes.NewReader(body), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
				t.Fatalf("put manifest %s: %v", mkey, err)
			}

			// Tombstone kind from the high bits of a byte: 0=none (live),
			// 1=young (within grace), 2=old (past grace), 3=zero-mtime
			// (backend hides mtime → gc conservatively treats as young).
			tombKind := int(at(i*4+1) >> 6)
			live := true
			if tombKind != 0 {
				tkey := mkey + ".tombstone"
				if _, err := sp.Put(ctx, tkey, bytes.NewReader([]byte("{}")), storage.PutOptions{ContentLength: 2}); err != nil {
					t.Fatalf("put tombstone %s: %v", tkey, err)
				}
				switch tombKind {
				case 1:
					ages[tkey] = base.Add(-grace / 2) // After cutoff → young
				case 2:
					ages[tkey] = base.Add(-2 * grace) // Before cutoff → old
					live = false
				case 3:
					ages[tkey] = time.Time{} // zero → treated young
				}
			}
			if live {
				for _, c := range chunks {
					expected[c] = struct{}{}
				}
			}
		}

		wrapped := &agedPlugin{StoragePlugin: sp, ages: ages}
		refs, err := repo.CollectReferencesWithOptions(ctx, wrapped, repo.CollectReferencesOptions{
			TombstoneGrace: grace,
			Now:            base,
		})
		if err != nil {
			t.Fatalf("CollectReferencesWithOptions: %v", err)
		}
		_ = graceCutoff

		// The invariant, checked in BOTH directions over the whole pool:
		// present in the ref set IFF referenced by a live/within-grace backup.
		for k := 0; k < poolSize; k++ {
			c := hashOf(k)
			h, perr := repo.ParseHash(c)
			if perr != nil {
				t.Fatalf("ParseHash(%s): %v", c, perr)
			}
			_, want := expected[c]
			got := refs.Has(h)
			switch {
			case want && !got:
				t.Fatalf("chunk %s is referenced by a LIVE or within-grace tombstoned backup but is "+
					"ABSENT from the ref set — gc would reap it, destroying a restorable/undeletable "+
					"backup (DATA LOSS)", c)
			case !want && got:
				t.Fatalf("chunk %s is referenced ONLY by past-grace tombstoned backups yet is RETAINED "+
					"in the ref set — a reclamation leak / grace-boundary bug", c)
			}
		}
	})
}
