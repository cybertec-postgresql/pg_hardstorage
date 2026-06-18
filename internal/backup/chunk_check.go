// chunk_check.go — manifest chunk-existence audit (present/missing partition).
package backup

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// ChunkCheckResult records a manifest's chunk-existence check.
// Used by `verify --existence-only` (already shipped via the
// CLI helper in c2834eb) and by `backup undelete --check-chunks`
// (this package's caller). The shape is intentionally minimal:
// just the totals + the list of missing hashes, so both callers
// can wrap it for their own structured output.
type ChunkCheckResult struct {
	// TotalUnique is the number of distinct chunk hashes
	// referenced by the manifest. Equal to "the count of
	// CAS reads a full restore would need."
	TotalUnique int

	// Present + Missing partition the unique hash set; their
	// sum equals TotalUnique. Missing is the actionable
	// signal — these chunk hashes are referenced by the
	// manifest but not present in the repo.
	Present int
	Missing []repo.Hash
}

// AllPresent reports whether every chunk referenced by the
// manifest exists in the repo. The natural pre-flight predicate
// for "is this backup still restorable?" — true means yes (or
// at least the bytes are addressable; integrity is a separate
// concern).
func (r *ChunkCheckResult) AllPresent() bool {
	return r != nil && len(r.Missing) == 0
}

// CheckChunkExistence walks every unique chunk hash in m's
// Files and Stat's the corresponding storage key. Returns a
// ChunkCheckResult enumerating present + missing.
//
// Trade-off: a chunk whose body has been silently corrupted
// (bit-rot, partial write that managed to register an Object)
// counts as "Present" here. Operators who want bit-integrity
// run the default `verify` (round-trip read + SHA-check); this
// helper is the 100x-faster "are the chunks even there?"
// pre-flight that pairs with `backup undelete` + future
// quick-restorability dashboards.
//
// Determinism: missing hashes returned in sorted order so
// downstream callers' output (CLI body, audit body) is stable
// across runs. Context cancellation surfaces immediately;
// partial walks return the partial Result + ctx.Err().
func CheckChunkExistence(ctx context.Context, sp storage.StoragePlugin, m *Manifest) (*ChunkCheckResult, error) {
	if sp == nil {
		return nil, errors.New("backup: CheckChunkExistence requires a non-nil StoragePlugin")
	}
	if m == nil {
		return nil, errors.New("backup: CheckChunkExistence requires a non-nil Manifest")
	}
	uniq := make(map[repo.Hash]struct{}, 64)
	for _, f := range m.Files {
		for _, c := range f.Chunks {
			uniq[c.Hash] = struct{}{}
		}
	}
	res := &ChunkCheckResult{TotalUnique: len(uniq)}

	hashes := make([]repo.Hash, 0, len(uniq))
	for h := range uniq {
		hashes = append(hashes, h)
	}
	sort.Slice(hashes, func(i, j int) bool {
		return hashes[i].String() < hashes[j].String()
	})

	for _, h := range hashes {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		_, err := sp.Stat(ctx, repo.ChunkKey(h))
		if err == nil {
			res.Present++
			continue
		}
		if errors.Is(err, storage.ErrNotFound) {
			res.Missing = append(res.Missing, h)
			continue
		}
		// Real backend error (network, permission). Surface
		// it — we can't tell whether the chunk is there or
		// not, and answering "missing" would be wrong.
		return res, fmt.Errorf("backup: stat chunk %s: %w", h, err)
	}
	return res, nil
}
