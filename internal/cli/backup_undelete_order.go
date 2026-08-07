//go:build !mutation_undelete_argv_order

package cli

import (
	"context"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// orderAncestorsFirst returns ids reordered so that, within the batch,
// every backup precedes its descendants. Order among independent ids —
// and everything else about the sequence — follows the caller's
// original order, so single-ID and unrelated-ID calls are untouched.
//
// The store refuses to resurrect an incremental under a tombstoned
// ancestor, and `backup delete --cascade` hands back its
// cascade_deleted slice LEAF-FIRST. This reordering is what makes
// "pass that slice straight back" — the unwind the documentation
// promises — actually work.
//
// Best-effort by design: a manifest that cannot be read contributes no
// edge and stays in caller order, and the per-ID loop then surfaces the
// real error for it. A parent outside the batch contributes no edge
// either — if that parent is still tombstoned, no order makes the
// resurrection safe, and the store's refusal stands.
func orderAncestorsFirst(ctx context.Context, store *backup.ManifestStore, deployment string, ids []string, verifier *backup.Verifier) []string {
	inBatch := make(map[string]bool, len(ids))
	for _, id := range ids {
		inBatch[id] = true
	}
	parent := make(map[string]string, len(ids))
	for _, id := range ids {
		m, _, err := store.ReadIncludingTombstoned(ctx, deployment, id, verifier)
		if err != nil || m == nil {
			continue
		}
		if m.ParentBackupID != "" && inBatch[m.ParentBackupID] {
			parent[id] = m.ParentBackupID
		}
	}
	if len(parent) == 0 {
		return ids
	}
	ordered := make([]string, 0, len(ids))
	emitted := make(map[string]bool, len(ids))
	// Stable Kahn walk: each pass emits, in caller order, every id
	// whose in-batch parent has been emitted. Bounded by len(ids)
	// passes; anything left after that (a cycle, which commit-time
	// validation makes impossible) falls back to caller order.
	for pass := 0; pass < len(ids); pass++ {
		progressed := false
		for _, id := range ids {
			if emitted[id] {
				continue
			}
			if p, has := parent[id]; has && !emitted[p] {
				continue
			}
			ordered = append(ordered, id)
			emitted[id] = true
			progressed = true
		}
		if !progressed || len(ordered) == len(ids) {
			break
		}
	}
	for _, id := range ids {
		if !emitted[id] {
			ordered = append(ordered, id)
		}
	}
	return ordered
}
