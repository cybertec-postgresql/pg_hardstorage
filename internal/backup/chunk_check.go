// chunk_check.go — manifest chunk-existence audit (present/missing partition).
package backup

import (
	"context"
	"errors"
	"fmt"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
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
	// Missing holds the first maxCheckMissingListed missing hashes in
	// ascending order; MissingCount is the exact number (a backup whose
	// storage is gone is missing hundreds of millions of chunks, and
	// listing each would exhaust memory).
	Missing      []repo.Hash
	MissingCount int
}

// maxCheckMissingListed bounds ChunkCheckResult.Missing.
var maxCheckMissingListed = 1000

// NumMissing is the exact number of missing chunks.
func (r *ChunkCheckResult) NumMissing() int {
	if r == nil {
		return 0
	}
	return max(r.MissingCount, len(r.Missing))
}

// AllPresent reports whether every chunk referenced by the
// manifest exists in the repo. The natural pre-flight predicate
// for "is this backup still restorable?" — true means yes (or
// at least the bytes are addressable; integrity is a separate
// concern).
func (r *ChunkCheckResult) AllPresent() bool {
	return r != nil && r.NumMissing() == 0
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
	// A bounded-memory set: a 200 TB backup references billions of
	// distinct chunks, far more than a map holds. ForEach yields them in
	// ascending order, which is also the sorted-hex order the result
	// promises.
	uniq := spill.NewHashSet(spill.Options{})
	defer uniq.Close()
	if err := m.EachFile(ctx, func(f *FileEntry) error {
		for _, c := range f.Chunks {
			uniq.Add(c.Hash)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := uniq.Err(); err != nil {
		return nil, fmt.Errorf("backup: chunk set: %w", err)
	}
	res := &ChunkCheckResult{TotalUnique: uniq.Len()}

	err := uniq.ForEach(func(k spill.Key) error {
		h := repo.Hash(k)
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := sp.Stat(ctx, repo.ChunkKey(h))
		if err == nil {
			res.Present++
			return nil
		}
		if errors.Is(err, storage.ErrNotFound) {
			res.MissingCount++
			if len(res.Missing) < maxCheckMissingListed {
				res.Missing = append(res.Missing, h)
			}
			return nil
		}
		// Real backend error (network, permission). Surface
		// it — we can't tell whether the chunk is there or
		// not, and answering "missing" would be wrong.
		return fmt.Errorf("backup: stat chunk %s: %w", h, err)
	})
	if err != nil {
		return res, err
	}
	return res, nil
}
