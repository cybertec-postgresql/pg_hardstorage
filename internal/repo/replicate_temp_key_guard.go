//go:build !mutation_replicate_temp_fullkey

package repo

import "strings"

// replicate_temp_key_guard.go — tmpMarkerInBasename reports whether a storage
// key is an in-flight commit staging temp (`<realname>.tmp.<rand>`), matched
// in the BASENAME only. Replicate and the replica-verify walk must skip
// staging temps but MUST NOT skip a committed manifest/segment whose backup
// ID or deployment NAME contains ".tmp." — validateStorageID permits dots, so
// `.../backups/db1.full.tmp.abc/manifest.json` and `wal/dep.tmp.x/...` are
// legal. A full-key `Contains(key, ".tmp.")` skipped those committed objects,
// so the DR replica silently omitted a backup / segment (data loss on a DR
// failover; replica-verify then reported "consistent" against a repo missing
// them). No committed object's basename contains ".tmp." (they end in
// manifest.json / <24hex>.json / <tli>.history / .backup / .partial), so
// basename scoping keeps them while still catching genuine temps. Own file so
// the mutation registry can carry the pre-fix full-key variant.
func tmpMarkerInBasename(key string) bool {
	base := key
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	return strings.Contains(base, ".tmp.")
}
