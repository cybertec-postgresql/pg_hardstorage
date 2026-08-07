//go:build !mutation_undelete_no_postflip_check

package backup

import "context"

// recheckResurrected re-verifies a just-resurrected manifest's chunks
// AT THE VISIBILITY POINT — after the tombstone marker is removed. The
// pre-flight ran while the manifest was still hidden, and a concurrent
// `repo gc --apply` sweep works from an older reference snapshot, so
// only this check can see chunks that vanished in the window. Own file
// so the mutation harness can swap in the pre-983dc4e variant.
func (ms *ManifestStore) recheckResurrected(ctx context.Context, m *Manifest) (*ChunkCheckResult, error) {
	return CheckChunkExistence(ctx, ms.sp, m)
}
