//go:build mutation_replicate_temp_fullkey

package repo

import "strings"

// MUTANT: full-key match (pre-fix). A committed backup/segment under a dotted
// ID is skipped by Replicate, so the DR replica silently omits it. Caught by
// TestReplicate_CopiesBackupWithTmpInID.
func tmpMarkerInBasename(key string) bool {
	return strings.Contains(key, ".tmp.")
}
