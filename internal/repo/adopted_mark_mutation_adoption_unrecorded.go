//go:build mutation_adoption_unrecorded

package repo

// markAdopted — MUTATED variant: adoption is not recorded, the exact
// pre-c31688b world. The commit-time gates then have nothing to
// re-Stat: a backup or WAL segment can commit a manifest referencing a
// chunk `repo gc --apply` swept mid-flight, born unrestorable while
// reporting success. Caught by
// TestVerifyAdoptedChunks_SweptAdoptionRefusesCommit,
// TestAdoptedHashes_LostPutRaceIsAdopted (backup/runner) and
// TestSink_AdoptedChunkSweptMidStream_RefusesCommit (pg/walsink).
func (c *CAS) markAdopted(h Hash) {
	_ = h
}
