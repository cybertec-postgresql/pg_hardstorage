//go:build linux

package spill

import "golang.org/x/sys/unix"

const (
	tmpfsMagic = 0x01021994
	ramfsMagic = 0x858458f6
)

// inMemoryFS reports whether dir is on tmpfs or ramfs, where "spilling to
// disk" actually spends RAM.
func inMemoryFS(dir string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return false
	}
	t := int64(st.Type)
	return t == tmpfsMagic || t == ramfsMagic
}
