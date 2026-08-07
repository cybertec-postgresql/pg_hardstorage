//go:build !mutation_adoption_unrecorded

package repo

// markAdopted records that this CAS instance deduplicated against hash
// without writing it — the input to the commit-time gates that close
// the dedup-vs-GC race (backup runner and walsink both re-Stat exactly
// these before publishing a manifest). Own file so the mutation
// harness can swap in the pre-c31688b no-op.
func (c *CAS) markAdopted(h Hash) {
	c.adopted.Store(h, struct{}{})
}
