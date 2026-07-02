//go:build !unix

package restore

import "os"

// stagingForeignOwner is a no-op on non-unix platforms: there is no POSIX
// uid to compare against. secureStagingDir still enforces a private,
// exclusively-created staging directory under a 0700 parent; ACL-based
// ownership verification on Windows is a future refinement.
func stagingForeignOwner(info os.FileInfo) (uid int, foreign bool) {
	return 0, false
}
