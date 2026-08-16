//go:build mutation_inventory_temp_fullkey

package inventory

import "strings"

// MUTANT: matches the staging marker anywhere in the FULL key (pre-fix). A
// committed segment under a deployment named `db.json.tmp.x` is skipped, so
// HighestArchivedLSN reports nothing archived and gap detection goes blind.
// Caught by TestFrontier_UnderDottedDeployment.
func segmentKeyIsStagingTemp(key string) bool {
	return strings.Contains(key, ".json.tmp.")
}
