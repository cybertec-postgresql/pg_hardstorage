package cli

import "strings"

// segmentKeyBasenameIsTemp reports whether a WAL/manifest key is an in-flight
// commit staging temp (`<name>.json.tmp.<rand>`), matched in the BASENAME
// only. A full-key match would false-skip committed objects under a
// deployment/backup ID that legitimately contains ".json.tmp." (validate
// StorageID permits dots) — see the repo-package guards (isStaleTempKey,
// segmentKeyIsStagingTemp) for the same fix on the same class.
func segmentKeyBasenameIsTemp(key string) bool {
	base := key
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	return strings.Contains(base, ".json.tmp.")
}
