//go:build unix

package restore

import (
	"os"
	"syscall"
)

// stagingForeignOwner reports whether a pre-existing staging directory is
// owned by a different user than the current process — the ownership half
// of secureStagingDir's reuse check. POSIX uid comparison; the non-unix
// build returns false (ownership there is enforced by the private parent
// directory + exclusive Mkdir instead, since Windows has no POSIX uid).
func stagingForeignOwner(info os.FileInfo) (uid int, foreign bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	if uint64(st.Uid) != uint64(os.Getuid()) {
		return int(st.Uid), true
	}
	return int(st.Uid), false
}
