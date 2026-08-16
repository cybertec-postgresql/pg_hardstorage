//go:build !mutation_inventory_temp_fullkey

package inventory

import "strings"

// segment_key_guard.go — segmentKeyIsStagingTemp reports whether a WAL
// segment-manifest key is an in-flight commit staging temp
// (`<seg>.json.tmp.<rand>`), matched in the BASENAME only.
//
// The frontier walks (HighestArchivedLSN via highestSegmentKey,
// FirstWALHoleInRange, NextArchivedLSNAtOrAfter) must skip staging temps but
// MUST NOT skip a committed segment whose DEPLOYMENT name contains the literal
// `.json.tmp.` — validateStorageID permits dots, so `wal/db.json.tmp.x/...`
// is a legal path. A full-key `Contains(key, ".json.tmp.")` skipped every
// segment of such a deployment, so HighestArchivedLSN returned found=false
// ("nothing archived") — blinding gap detection (a failover gap is silently
// missed, bug-#2 class) and giving restore the wrong bounds. Basename scoping
// fixes it: a committed segment's basename is `<24hex>.json`, carrying no
// `.tmp.` marker. Own file so the mutation registry can carry the pre-fix
// full-key variant (mutation_inventory_temp_fullkey).
func segmentKeyIsStagingTemp(key string) bool {
	base := key
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	return strings.Contains(base, ".json.tmp.")
}
