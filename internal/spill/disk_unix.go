//go:build unix

package spill

import "golang.org/x/sys/unix"

// freeBytes reports the space an unprivileged process can still write in
// the volume holding dir. ok is false when the platform cannot say.
func freeBytes(dir string) (free int64, ok bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}
