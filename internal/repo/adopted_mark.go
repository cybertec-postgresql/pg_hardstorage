//go:build !mutation_adoption_unrecorded

package repo

import "fmt"

// markAdopted records that this CAS instance deduplicated against hash
// without writing it — the input to the commit-time gates that close
// the dedup-vs-GC race (backup runner and walsink both re-Stat exactly
// these before publishing a manifest). Own file so the mutation
// harness can swap in the pre-c31688b no-op.
//
// It returns the spilled set's I/O error, if any: a set that lost an
// adoption would shrink the commit gate silently, and one on a full disk
// is not going to recover, so the caller fails the chunk (and with it the
// backup) now instead of after the rest of the data has been read.
func (c *CAS) markAdopted(h Hash) error {
	if c.adoptedSpill != nil {
		c.adoptedGone.Delete(h)
		c.adoptedSpill.Add(h)
		if err := c.adoptedSpill.Err(); err != nil {
			return fmt.Errorf("cas: adoption set: %w", err)
		}
		return nil
	}
	c.adopted.Store(h, struct{}{})
	return nil
}
