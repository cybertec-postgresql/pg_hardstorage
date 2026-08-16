//go:build !mutation_stale_temp_fullkey

package repo

import "strings"

// stale_temp_key_guard.go — isStaleTempKey decides whether a listed key is
// an orphaned commit staging file (`<realkey>.tmp.<rand>`, realkey ending in
// .json or .history) that FindStaleTempManifests may reap.
//
// The match MUST be scoped to the basename (final path component). The commit
// writer appends `.tmp.<rand>` to a real key, so a genuine temp always
// carries the `.json.tmp.` / `.history.tmp.` marker in its LAST component
// (`manifest.json.tmp.<rand>`). A COMMITTED manifest whose parent path merely
// contains the literal `.json.tmp.` — reachable because validateStorageID
// permits dots in deployment/backup IDs, e.g.
// `manifests/db1/backups/evil.json.tmp.x/manifest.json` or a deployment named
// `db.history.tmp.z` — would match a full-key Contains and be reaped by
// `repo gc --apply`, SILENTLY DELETING a live backup's manifest (data loss).
// Basename scoping excludes that: the committed file's basename is
// `manifest.json`, which carries no `.tmp.` marker.
//
// Own file so the mutation registry can carry the pre-fix full-key variant
// (mutation_stale_temp_fullkey) that reopens the hole.
func isStaleTempKey(key string) bool {
	base := key
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	return strings.Contains(base, ".json.tmp.") || strings.Contains(base, ".history.tmp.")
}
