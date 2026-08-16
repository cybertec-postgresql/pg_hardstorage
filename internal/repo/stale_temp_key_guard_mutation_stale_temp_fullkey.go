//go:build mutation_stale_temp_fullkey

package repo

import "strings"

// stale_temp_key_guard_mutation_stale_temp_fullkey.go — MUTANT: matches the
// staging marker anywhere in the FULL key (the pre-fix behaviour). A
// committed manifest under a deployment/backup ID that contains `.json.tmp.`
// or `.history.tmp.` is then flagged stale and deleted by `repo gc --apply`
// — silent data loss. Caught by TestFindStaleTemp_NeverFlagsLiveManifest.
func isStaleTempKey(key string) bool {
	return strings.Contains(key, ".json.tmp.") || strings.Contains(key, ".history.tmp.")
}
