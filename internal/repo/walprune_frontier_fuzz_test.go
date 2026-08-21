package repo_test

// walprune_frontier_fuzz_test.go — the WAL-SEGMENT retention data-loss
// invariant, fuzzed. This is the sibling of the gc reference/grace fuzzes:
// they cover CHUNK retention, this covers WAL-SEGMENT retention.
//
// wal prune deletes a WAL segment manifest when its end_lsn is strictly
// below the frontier — the MINIMUM start_lsn over every backup that is live
// or within-grace tombstoned. PITR replays WAL forward from a kept backup's
// position, so a segment at or above that minimum may still be needed. The
// invariant:
//
//   frontier == min(start_lsn) over {live ∪ young-tombstoned} backups, and
//   a segment is deleted IFF its end_lsn < frontier.
//
// The dangerous direction is a frontier computed TOO HIGH: it advances past
// a backup that still needs older WAL, and prune deletes that WAL — a
// silent, unrecoverable PITR hole (restore later halts / promotes behind).
// This is example-tested (min-start-lsn-not-oldest-stopped, young/old
// tombstone, grace-disabled) but never fuzzed over arbitrary many backups ×
// start_lsns × tombstone ages, which is where an ordering/tie/edge bug in
// oldestKeptBackupFrontier would hide.
//
// The expected frontier is computed independently, so asserting the segment
// delete-set against it is NOT tautological with WALPrune's own end_lsn <
// frontier branch: a wrong frontier deletes a segment this check rejects.
// Per-tombstone ages are driven through the same List-wrapping agedPlugin the
// gc fuzzes use; manifests are written directly (WALPrune partial-decodes
// JSON, no CAS needed).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func FuzzWALPruneNeverDeletesWALBelowKeptBackupFrontier(f *testing.F) {
	f.Add([]byte{5, 3, 0, 7, 1, 2, 130, 4, 66, 0, 9, 1, 3, 200}, []byte{2, 1, 4, 8, 6})
	f.Add([]byte{3, 1, 0, 2, 0, 5, 0}, []byte{1, 3, 7})
	f.Fuzz(func(t *testing.T, braw, sraw []byte) {
		if len(braw) < 2 || len(sraw) < 1 {
			return
		}
		ctx := context.Background()
		sp := &fs.Plugin{}
		if err := sp.Open(ctx, storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
			t.Fatalf("open fs: %v", err)
		}
		t.Cleanup(func() { _ = sp.Close() })

		base := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
		grace := time.Hour
		put := func(key string, body []byte) {
			if _, err := sp.Put(ctx, key, bytes.NewReader(body), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
				t.Fatalf("put %s: %v", key, err)
			}
		}
		// Distinct, nonzero, spaced LSNs (0 is WALPrune's "no frontier"
		// sentinel, so every start_lsn must be > 0).
		lsnOf := func(k int) pglogrepl.LSN { return pglogrepl.LSN(uint64(k%64+1) * 0x1000000) }
		bat := func(i int) byte {
			if i < 0 {
				i = -i
			}
			return braw[i%len(braw)]
		}

		ages := map[string]time.Time{}
		var expFrontier pglogrepl.LSN
		haveKept := false

		nb := int(braw[0]%16) + 1
		for i := 0; i < nb; i++ {
			id := fmt.Sprintf("bk%03d", i)
			startLSN := lsnOf(int(bat(i*2 + 1)))
			mkey := "manifests/db1/backups/" + id + "/manifest.json"
			body, _ := json.Marshal(struct {
				BackupID string `json:"backup_id"`
				StartLSN string `json:"start_lsn"`
			}{id, startLSN.String()})
			put(mkey, body)

			kept := true
			switch int(bat(i*2+2)) % 4 {
			case 0: // live, no tombstone
			case 1: // young tombstone → kept
				put(mkey+".tombstone", []byte("{}"))
				ages[mkey+".tombstone"] = base.Add(-grace / 2)
			case 2: // past-grace tombstone → excluded
				put(mkey+".tombstone", []byte("{}"))
				ages[mkey+".tombstone"] = base.Add(-2 * grace)
				kept = false
			case 3: // zero-mtime tombstone → young while grace active → kept
				put(mkey+".tombstone", []byte("{}"))
				ages[mkey+".tombstone"] = time.Time{}
			}
			if kept && (!haveKept || startLSN < expFrontier) {
				expFrontier = startLSN
				haveKept = true
			}
		}

		segEndByName := map[string]pglogrepl.LSN{}
		ns := int(sraw[0]%12) + 1
		var lastEnd pglogrepl.LSN
		for j := 0; j < ns; j++ {
			end := lsnOf(int(sraw[j%len(sraw)]))
			// Segment names sort in WAL order and a WAL is contiguous,
			// so end_lsn is non-decreasing along the names. Enforce that
			// monotonicity — the premise WALPrune's ordering proof (skip
			// the retained suffix without reading its manifests) rests
			// on — while keeping the values random.
			if end < lastEnd {
				end = lastEnd + 0x1000000
			}
			lastEnd = end
			name := fmt.Sprintf("seg%04d", j)
			body, _ := json.Marshal(struct {
				StartLSN  string    `json:"start_lsn"`
				EndLSN    string    `json:"end_lsn"`
				CreatedAt time.Time `json:"created_at"`
			}{(end - 0x100000).String(), end.String(), base})
			put("wal/db1/1/"+name+".json", body)
			segEndByName[name] = end
		}

		wrapped := &agedPlugin{StoragePlugin: sp, ages: ages}
		var mu sync.Mutex
		wouldDelete := map[string]struct{}{}
		res, err := repo.WALPrune(ctx, wrapped, repo.WALPruneOptions{
			Deployment:     "db1",
			DryRun:         true,
			TombstoneGrace: grace,
			Now:            base,
			OnProgress: func(ev repo.WALPruneProgress) {
				if ev.Outcome == "would_delete" {
					mu.Lock()
					wouldDelete[ev.SegmentName] = struct{}{}
					mu.Unlock()
				}
			},
		})
		if err != nil {
			t.Fatalf("WALPrune: %v", err)
		}

		if !haveKept {
			// No live/young backup → no frontier → WALPrune must keep all.
			if res.FrontierLSN != "" || len(wouldDelete) != 0 {
				t.Fatalf("no kept backup but frontier=%q, %d segments would be deleted — must keep everything",
					res.FrontierLSN, len(wouldDelete))
			}
			return
		}

		gotF, perr := pglogrepl.ParseLSN(res.FrontierLSN)
		if perr != nil {
			t.Fatalf("parse frontier %q: %v", res.FrontierLSN, perr)
		}
		if gotF != expFrontier {
			t.Fatalf("frontier=%s want=%s — oldestKeptBackupFrontier != min(start_lsn) over "+
				"live+young-tombstoned backups", gotF, expFrontier)
		}

		// A segment is deleted IFF end_lsn < frontier. Checked against the
		// INDEPENDENT expected frontier, so a wrong frontier is caught here
		// even though WALPrune's own branch is end_lsn < its frontier.
		for name, end := range segEndByName {
			_, del := wouldDelete[name]
			wantDel := end < expFrontier
			switch {
			case wantDel && !del:
				t.Fatalf("segment %s (end_lsn=%s) < frontier=%s but was KEPT — under-prune (reclamation leak)",
					name, end, expFrontier)
			case del && !wantDel:
				t.Fatalf("segment %s (end_lsn=%s) >= frontier=%s was DELETED — WAL needed to replay forward "+
					"from a kept backup was pruned (silent PITR hole, DATA LOSS)", name, end, expFrontier)
			}
		}
	})
}
