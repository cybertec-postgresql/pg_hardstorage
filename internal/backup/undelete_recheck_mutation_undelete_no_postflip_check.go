//go:build mutation_undelete_no_postflip_check

package backup

import "context"

// recheckResurrected — MUTATED variant: reports every chunk present
// without looking, the exact pre-983dc4e world. An undelete racing
// gc's sweep then returns restored=true for a backup whose chunks are
// gone — a phantom restore point that --check-chunks explicitly
// vouched for. Caught by
// TestUndelete_SweptDuringUndelete_RetombstonesAndRefuses.
func (ms *ManifestStore) recheckResurrected(ctx context.Context, m *Manifest) (*ChunkCheckResult, error) {
	_ = ctx
	_ = m
	return &ChunkCheckResult{}, nil // len(Missing)==0 ⇒ AllPresent
}
